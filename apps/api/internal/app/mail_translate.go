package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/html"
)

const translateCooldown = 10 * time.Second

var (
	translateMu      sync.Mutex
	translateLastReq = make(map[string]time.Time)
)

func allowTranslate(userID string) bool {
	translateMu.Lock()
	defer translateMu.Unlock()
	if last, ok := translateLastReq[userID]; ok && time.Since(last) < translateCooldown {
		return false
	}
	translateLastReq[userID] = time.Now()
	return true
}

const siliconflowEndpoint = "https://api.siliconflow.cn/v1/chat/completions"
const siliconflowDefaultModel = "deepseek-ai/DeepSeek-V3"

const translateChunkSize = 2000
const translateMaxConcurrency = 3

type translateMailMessageRequest struct {
	TargetLanguage string `json:"targetLanguage"`
}

type translateMailMessageResponse struct {
	TranslatedText    string `json:"translatedText"`
	TranslatedSubject string `json:"translatedSubject,omitempty"`
	TranslatedHTML    string `json:"translatedHtml,omitempty"`
	SourceLanguage    string `json:"sourceLanguage,omitempty"`
	TargetLanguage    string `json:"targetLanguage"`
	Truncated         bool   `json:"truncated"`
}

type siliconflowRequest struct {
	Model          string          `json:"model"`
	Messages       []siliconflowMsg `json:"messages"`
	EnableThinking *bool           `json:"enable_thinking,omitempty"`
}

type siliconflowMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type siliconflowResponse struct {
	Choices []siliconflowChoice `json:"choices"`
}

type siliconflowChoice struct {
	Message siliconflowMsg `json:"message"`
}

type openrouterRequest = siliconflowRequest
type openrouterMsg = siliconflowMsg
type openrouterResponse = siliconflowResponse
type openrouterChoice = siliconflowChoice
const openrouterEndpoint = siliconflowEndpoint
const openrouterModel = siliconflowDefaultModel

func (a *App) handleTranslateMailMessage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	user := currentUser(r)
	if user != nil && !allowTranslate(user.ID) {
		respondError(w, http.StatusTooManyRequests, "translation requests too frequent, please wait")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	msg, err := a.loadMessageForReadRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	text := strings.TrimSpace(msg.BodyText)
	if text == "" {
		text = strings.TrimSpace(msg.Snippet)
	}
	if text == "" {
		respondError(w, http.StatusBadRequest, "message has no translatable text")
		return
	}
	maxChars := a.cfg.MailTranslateMaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	text, truncated := truncateRunes(text, maxChars)

	apiKey := a.cfg.SiliconFlowAPIKey
	if apiKey == "" {
		apiKey = a.cfg.OpenRouterAPIKey
	}
	model := a.cfg.SiliconFlowModel
	if model == "" {
		model = siliconflowDefaultModel
	}

	translated, source, err := translateChunkedWithModel(r.Context(), apiKey, model, text, target)
	if err != nil {
		a.log.Warn("mail translation failed", "message_id", msg.ID, "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	translatedHTML := ""
	if strings.TrimSpace(msg.BodyHTML) != "" {
		translatedHTML, _ = translateHTMLTextNodesWithModel(r.Context(), a.policy, apiKey, model, msg.BodyHTML, target, maxChars)
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedText: translated, TranslatedHTML: translatedHTML, SourceLanguage: source, TargetLanguage: target, Truncated: truncated})
}

func (a *App) handleTranslateExternalIMAPMessage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	user := currentUser(r)
	if user != nil && !allowTranslate(user.ID) {
		respondError(w, http.StatusTooManyRequests, "translation requests too frequent, please wait")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	stored, _, err := a.parseMaildirMessage(raw, account.Username)
	text := ""
	if err == nil {
		text = strings.TrimSpace(stored.BodyText)
		if text == "" {
			text = strings.TrimSpace(stored.Snippet)
		}
	}
	if text == "" {
		text = strings.TrimSpace(remote.Snippet)
	}
	if text == "" {
		respondError(w, http.StatusBadRequest, "message has no translatable text")
		return
	}
	maxChars := a.cfg.MailTranslateMaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	text, truncated := truncateRunes(text, maxChars)

	apiKey := a.cfg.SiliconFlowAPIKey
	if apiKey == "" {
		apiKey = a.cfg.OpenRouterAPIKey
	}
	model := a.cfg.SiliconFlowModel
	if model == "" {
		model = siliconflowDefaultModel
	}

	translated, source, err := translateChunkedWithModel(r.Context(), apiKey, model, text, target)
	if err != nil {
		a.log.Warn("external mail translation failed", "account_id", account.ID, "remote_id", chi.URLParam(r, "remoteId"), "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	translatedHTML := ""
	if err == nil && strings.TrimSpace(stored.BodyHTML) != "" {
		translatedHTML, _ = translateHTMLTextNodesWithModel(r.Context(), a.policy, apiKey, model, stored.BodyHTML, target, maxChars)
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedText: translated, TranslatedHTML: translatedHTML, SourceLanguage: source, TargetLanguage: target, Truncated: truncated})
}

func (a *App) handleTranslateMailSubject(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	msg, err := a.loadMessageForReadRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	subj := strings.TrimSpace(msg.Subject)
	if subj == "" || subj == "(no subject)" || !containsTranslatableLetter(subj) {
		respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedSubject: subj, SourceLanguage: detectSourceLanguage(subj), TargetLanguage: target})
		return
	}
	apiKey := a.cfg.SiliconFlowAPIKey
	if apiKey == "" {
		apiKey = a.cfg.OpenRouterAPIKey
	}
	model := a.cfg.SiliconFlowModel
	if model == "" {
		model = siliconflowDefaultModel
	}
	translated, source, err := openrouterTranslateWithModel(r.Context(), apiKey, model, subj, target)
	if err != nil {
		a.log.Warn("mail subject translation failed", "message_id", msg.ID, "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedSubject: translated, SourceLanguage: source, TargetLanguage: target})
}

func (a *App) handleTranslateExternalIMAPSubject(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	raw, _, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	stored, _, err := a.parseMaildirMessage(raw, account.Username)
	subj := ""
	if err == nil {
		subj = strings.TrimSpace(stored.Subject)
	}
	if subj == "" || subj == "(no subject)" || !containsTranslatableLetter(subj) {
		respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedSubject: subj, SourceLanguage: detectSourceLanguage(subj), TargetLanguage: target})
		return
	}
	apiKey := a.cfg.SiliconFlowAPIKey
	if apiKey == "" {
		apiKey = a.cfg.OpenRouterAPIKey
	}
	model := a.cfg.SiliconFlowModel
	if model == "" {
		model = siliconflowDefaultModel
	}
	translated, source, err := openrouterTranslateWithModel(r.Context(), apiKey, model, subj, target)
	if err != nil {
		a.log.Warn("external mail subject translation failed", "account_id", account.ID, "remote_id", chi.URLParam(r, "remoteId"), "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedSubject: translated, SourceLanguage: source, TargetLanguage: target})
}

func splitTextIntoChunks(text string, maxRunes int) []string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	start := 0
	for start < len(runes) {
		end := start + maxRunes
		if end >= len(runes) {
			chunks = append(chunks, string(runes[start:]))
			break
		}
		cut := end
		searchStart := end - 400
		if searchStart < start {
			searchStart = start
		}
		best := -1
		for i := end - 1; i >= searchStart; i-- {
			if i+1 < len(runes) && runes[i] == '\n' && runes[i+1] == '\n' {
				best = i + 2
				break
			}
		}
		if best == -1 {
			for i := end - 1; i >= searchStart; i-- {
				if runes[i] == '\n' {
					best = i + 1
					break
				}
			}
		}
		if best == -1 {
			for i := end - 1; i >= searchStart; i-- {
				if runes[i] == '.' && i+1 < len(runes) && runes[i+1] == ' ' {
					best = i + 1
					break
				}
			}
		}
		if best == -1 {
			for i := end - 1; i >= searchStart; i-- {
				if runes[i] == ' ' {
					best = i + 1
					break
				}
			}
		}
		if best != -1 && best > start {
			cut = best
		}
		chunk := string(runes[start:cut])
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
		start = cut
	}
	return chunks
}

func translateChunked(ctx context.Context, apiKey, text, target string) (string, string, error) {
	return translateChunkedWithModel(ctx, apiKey, siliconflowDefaultModel, text, target)
}

func translateChunkedWithModel(ctx context.Context, apiKey, model, text, target string) (string, string, error) {
	chunks := splitTextIntoChunks(text, translateChunkSize)
	if len(chunks) <= 1 {
		return openrouterTranslateWithModel(ctx, apiKey, model, text, target)
	}
	nonEmptyIdx := make([]int, 0, len(chunks))
	for i, c := range chunks {
		if strings.TrimSpace(c) != "" {
			nonEmptyIdx = append(nonEmptyIdx, i)
		}
	}
	if len(nonEmptyIdx) == 0 {
		return openrouterTranslateWithModel(ctx, apiKey, model, text, target)
	}
	results := make([]string, len(chunks))
	sources := make([]string, len(chunks))
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, translateMaxConcurrency)
	for _, idx := range nonEmptyIdx {
		ch := chunks[idx]
		wg.Add(1)
		go func(i int, content string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			txt, src, err := openrouterTranslateWithModel(ctx, apiKey, model, content, target)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err == nil {
				results[i] = txt
				sources[i] = src
			}
		}(idx, ch)
	}
	wg.Wait()
	if firstErr != nil {
		return "", "", firstErr
	}
	for i, c := range chunks {
		if strings.TrimSpace(c) == "" {
			results[i] = c
		}
	}
	joined := strings.Join(results, "")
	src := ""
	for _, s := range sources {
		if s != "" {
			src = s
			break
		}
	}
	if src == "" {
		src = detectSourceLanguage(text)
	}
	return strings.TrimSpace(joined), src, nil
}

func translateHTMLTextNodes(ctx context.Context, policy *HTMLPolicy, apiKey, bodyHTML, target string, maxChars int) (string, error) {
	return translateHTMLTextNodesWithModel(ctx, policy, apiKey, siliconflowDefaultModel, bodyHTML, target, maxChars)
}

func translateHTMLTextNodesWithModel(ctx context.Context, policy *HTMLPolicy, apiKey, model, bodyHTML, target string, maxChars int) (string, error) {
	nodes, err := html.ParseFragment(strings.NewReader(bodyHTML), nil)
	if err != nil {
		return "", err
	}
	type htmlJob struct {
		node    *html.Node
		orig    string
		limited string
	}
	var jobs []*htmlJob
	remaining := maxChars
	var collect func(*html.Node)
	collect = func(n *html.Node) {
		if n.Type == html.ElementNode && shouldSkipHTMLTranslationElement(n.Data) {
			return
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" && containsTranslatableLetter(text) && remaining > 0 {
				limited, _ := truncateRunes(text, remaining)
				remaining -= utf8.RuneCountInString(limited)
				jobs = append(jobs, &htmlJob{node: n, orig: text, limited: limited})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	for _, n := range nodes {
		collect(n)
	}
	if len(jobs) == 0 {
		var b bytes.Buffer
		for _, n := range nodes {
			if err := html.Render(&b, n); err != nil {
				return "", err
			}
		}
		if policy != nil {
			return policy.Sanitize(b.String()), nil
		}
		return b.String(), nil
	}
	sem := make(chan struct{}, translateMaxConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, job := range jobs {
		wg.Add(1)
		go func(j *htmlJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			translated, _, err := openrouterTranslateWithModel(ctx, apiKey, model, j.limited, target)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				return
			}
			if err == nil {
				j.node.Data = strings.Replace(j.node.Data, j.orig, translated, 1)
			}
		}(job)
	}
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	var b bytes.Buffer
	for _, n := range nodes {
		if err := html.Render(&b, n); err != nil {
			return "", err
		}
	}
	if policy != nil {
		return policy.Sanitize(b.String()), nil
	}
	return b.String(), nil
}

func shouldSkipHTMLTranslationElement(tag string) bool {
	switch strings.ToLower(tag) {
	case "script", "style", "code", "pre", "textarea":
		return true
	default:
		return false
	}
}

func containsTranslatableLetter(value string) bool {
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '\u4e00' && r <= '\u9fff') {
			return true
		}
	}
	return false
}

func normalizeTranslateTarget(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "zh", "zh-cn", "zh-hans", "zh_cn":
		return "zh-CN"
	case "zh-tw", "zh-hant", "zh_hk", "zh-hk", "zh-mo":
		return "zh-TW"
	case "en", "en-us", "en-gb":
		return "en"
	default:
		return ""
	}
}

func truncateRunes(value string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value, false
	}
	out := make([]rune, 0, max)
	for i, r := range value {
		if len(out) >= max {
			return string(out), i < len(value)
		}
		out = append(out, r)
	}
	return string(out), false
}

const openrouterSystemPrompt = "You are a professional translator. Translate the following text to {{TARGET}}. Requirements: Output ONLY the translated text, no explanations or notes. Preserve original markdown formatting and line breaks. Translate EVERY part including titles and headings. If already in target language, return as-is."

func openrouterTranslate(ctx context.Context, apiKey, text, target string) (string, string, error) {
	return openrouterTranslateWithModel(ctx, apiKey, siliconflowDefaultModel, text, target)
}

func openrouterTranslateWithModel(ctx context.Context, apiKey, model, text, target string) (string, string, error) {
	if apiKey == "" {
		return "", "", fmt.Errorf("translate api key not configured")
	}
	if strings.TrimSpace(text) == "" {
		return "", "", fmt.Errorf("empty text")
	}
	if model == "" {
		model = siliconflowDefaultModel
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second + time.Duration(rand.Intn(1000))*time.Millisecond
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		translated, source, err := doOpenrouterTranslateWithModel(ctx, apiKey, model, text, target)
		if err == nil {
			return translated, source, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "status 429") || strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 503") {
			continue
		}
		return "", "", err
	}
	return "", "", fmt.Errorf("translation failed after 3 attempts: %w", lastErr)
}

func doOpenrouterTranslate(ctx context.Context, apiKey, text, target string) (string, string, error) {
	return doOpenrouterTranslateWithModel(ctx, apiKey, siliconflowDefaultModel, text, target)
}

func doOpenrouterTranslateWithModel(ctx context.Context, apiKey, model, text, target string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	systemPrompt := strings.ReplaceAll(openrouterSystemPrompt, "{{TARGET}}", target)
	prompt := text
	enableThinking := false
	reqBody := siliconflowRequest{
		Model: model,
		Messages: []siliconflowMsg{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		EnableThinking: &enableThinking,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, siliconflowEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return "", "", fmt.Errorf("siliconflow status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var sfRes siliconflowResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 4*1024*1024)).Decode(&sfRes); err != nil {
		return "", "", err
	}
	if len(sfRes.Choices) == 0 || strings.TrimSpace(sfRes.Choices[0].Message.Content) == "" {
		return "", "", fmt.Errorf("empty translation response")
	}
	translated := strings.TrimSpace(sfRes.Choices[0].Message.Content)
	source := detectSourceLanguage(text)
	return translated, source, nil
}

func detectSourceLanguage(text string) string {
	for _, r := range text {
		if r >= '\u4e00' && r <= '\u9fff' {
			return "zh-CN"
		}
	}
	return "en"
}

func parseGoogleTranslateResponse(raw any) (string, string) {
	root, _ := raw.([]any)
	var b strings.Builder
	if len(root) > 0 {
		if sentences, ok := root[0].([]any); ok {
			for _, item := range sentences {
				parts, ok := item.([]any)
				if !ok || len(parts) == 0 {
					continue
				}
				if s, ok := parts[0].(string); ok {
					b.WriteString(s)
				}
			}
		}
	}
	source := ""
	if len(root) > 2 {
		if s, ok := root[2].(string); ok {
			source = s
		}
	}
	return b.String(), source
}
// Add these new functions and handler after the existing handleTranslateMailMessage

func (a *App) handleTranslateMailMessageStream(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	user := currentUser(r)
	if user != nil && !allowTranslate(user.ID) {
		respondError(w, http.StatusTooManyRequests, "translation requests too frequent, please wait")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	msg, err := a.loadMessageForReadRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	text := strings.TrimSpace(msg.BodyText)
	if text == "" {
		text = strings.TrimSpace(msg.Snippet)
	}
	if text == "" {
		respondError(w, http.StatusBadRequest, "message has no translatable text")
		return
	}
	maxChars := a.cfg.MailTranslateMaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	text, truncated := truncateRunes(text, maxChars)

	apiKey := a.cfg.SiliconFlowAPIKey
	if apiKey == "" {
		apiKey = a.cfg.OpenRouterAPIKey
	}
	model := a.cfg.SiliconFlowModel
	if model == "" {
		model = siliconflowDefaultModel
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Send initial metadata
	sendSSEEvent(w, flusher, "meta", map[string]any{
		"targetLanguage": target,
		"truncated":      truncated,
	})

	// Translate subject first (fast)
	subj := strings.TrimSpace(msg.Subject)
	if subj != "" && subj != "(no subject)" && containsTranslatableLetter(subj) {
		translatedSubject, source, err := openrouterTranslateWithModel(r.Context(), apiKey, model, subj, target)
		if err == nil {
			sendSSEEvent(w, flusher, "subject", map[string]any{
				"translatedSubject": translatedSubject,
				"sourceLanguage":    source,
			})
		}
	}

	// Stream body chunks
	err = translateChunkedStream(r.Context(), apiKey, model, text, target, func(idx int, chunk string, source string) {
		sendSSEEvent(w, flusher, "chunk", map[string]any{
			"index":      idx,
			"text":       chunk,
			"sourceLang": source,
		})
	})
	if err != nil {
		sendSSEEvent(w, flusher, "error", map[string]any{"message": err.Error()})
		return
	}

	// Send done event
	sendSSEEvent(w, flusher, "done", map[string]any{})
}

func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	flusher.Flush()
}

// translateChunkedStream calls callback for each completed chunk in order
func translateChunkedStream(ctx context.Context, apiKey, model, text, target string, onChunk func(int, string, string)) error {
	chunks := splitTextIntoChunks(text, translateChunkSize)
	if len(chunks) <= 1 {
		txt, src, err := openrouterTranslateWithModel(ctx, apiKey, model, text, target)
		if err != nil {
			return err
		}
		onChunk(0, txt, src)
		return nil
	}
	nonEmptyIdx := make([]int, 0, len(chunks))
	for i, c := range chunks {
		if strings.TrimSpace(c) != "" {
			nonEmptyIdx = append(nonEmptyIdx, i)
		}
	}
	if len(nonEmptyIdx) == 0 {
		txt, src, err := openrouterTranslateWithModel(ctx, apiKey, model, text, target)
		if err != nil {
			return err
		}
		onChunk(0, txt, src)
		return nil
	}

	// Use a channel to collect results in order
	resultChan := make(chan chunkResult, len(nonEmptyIdx))
	var wg sync.WaitGroup
	sem := make(chan struct{}, translateMaxConcurrency)
	var firstErr error
	var errMu sync.Mutex

	for _, idx := range nonEmptyIdx {
		ch := chunks[idx]
		wg.Add(1)
		go func(i int, content string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			txt, src, err := openrouterTranslateWithModel(ctx, apiKey, model, content, target)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			resultChan <- chunkResult{index: i, text: txt, source: src}
		}(idx, ch)
	}

	// Close channel when all done
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results in order
	results := make([]chunkResult, len(nonEmptyIdx))
	received := 0
	for res := range resultChan {
		results[res.index] = res
		received++
		// Send in order - wait for sequential indices
		for received > 0 && results[received-1].text != "" {
			onChunk(results[received-1].index, results[received-1].text, results[received-1].source)
			received--
		}
	}

	if firstErr != nil {
		return firstErr
	}
	return nil
}

type chunkResult struct {
	index  int
	text   string
	source string
}
