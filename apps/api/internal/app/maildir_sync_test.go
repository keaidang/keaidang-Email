package app

import (
	"strings"
	"testing"
)

func TestDecodeMIMEHeader_GB2312(t *testing.T) {
	// GB2312 encoded: <广告> 戴上 AirPods
	input := `=?gb2312?B?PLnjuOY+ILT3yc8gQWlyUG9kcw==?=`
	got := decodeMIMEHeader(input)
	want := "<广告> 戴上 AirPods"
	if got != want {
		t.Errorf("decodeMIMEHeader(gb2312) = %q, want %q", got, want)
	}
}

func TestDecodeMIMEHeader_GBK(t *testing.T) {
	input := `=?gbk?Q?=B9=E3=B8=E6_Mac_mini?=`
	got := decodeMIMEHeader(input)
	want := "苹果Mac mini"
	if got != want {
		t.Errorf("decodeMIMEHeader(gbk) = %q, want %q", got, want)
	}
}

func TestDecodeMIMEHeader_UTF8(t *testing.T) {
	input := `=?utf-8?B?5aOr5aOr5rWL6K+V?=`
	got := decodeMIMEHeader(input)
	want := "你好世界"
	if got != want {
		t.Errorf("decodeMIMEHeader(utf-8) = %q, want %q", got, want)
	}
}

func TestDecodeMIMEHeader_QEncodingUnderscoreToSpace(t *testing.T) {
	input := `=?gb2312?Q?hello_World?=`
	got := decodeMIMEHeader(input)
	want := "hello World"
	if got != want {
		t.Errorf("decodeMIMEHeader(Q _) = %q, want %q", got, want)
	}
}

func TestDecodeMIMEHeader_AdjacentWords(t *testing.T) {
	input := `=?gb2312?Q?Mac_mini?= =?gb2312?Q?tLG8NXLu6e71ri0x+vH8w==?=`
	got := decodeMIMEHeader(input)
	want := "苹果Mac mini 戴上 AirPods"
	if got != want {
		t.Errorf("decodeMIMEHeader(adjacent) = %q, want %q", got, want)
	}
}

func TestDecodeMIMEHeader_NoEncoding(t *testing.T) {
	input := "Plain Subject"
	got := decodeMIMEHeader(input)
	if got != input {
		t.Errorf("decodeMIMEHeader(plain) = %q, want %q", got, input)
	}
}

func TestDecodeMIMEHeader_GB18030(t *testing.T) {
	input := `=?gb18030?B?PLnjuOY+ILT3yc8gQWlyUG9kcw==?=`
	got := decodeMIMEHeader(input)
	if !strings.Contains(got, "广") && got == input {
		t.Errorf("decodeMIMEHeader(gb18030) should decode, got %q", got)
	}
}

func TestDecodeMIMEHeader_Big5(t *testing.T) {
	input := `=?big5?B?swCd2rHcswA=?=`
	got := decodeMIMEHeader(input)
	if got == "" {
		t.Errorf("decodeMIMEHeader(big5) should not be empty")
	}
}

func TestDecodeMIMEHeader_FallbackOnFailure(t *testing.T) {
	// When charsetReader fails, should not return empty or panic
	input := "=?unknown-charset?B?ABC==?="
	got := decodeMIMEHeader(input)
	// Should not be empty or crash
	if got == "" {
		t.Errorf("decodeMIMEHeader with unknown charset should not return empty")
	}
}

func TestCharsetReader_RecognizesChineseCharsets(t *testing.T) {
	testCases := []string{
		"gb2312", "gb2312-80", "gb2312-2000",
		"gbk", "gb18030",
		"big5",
	}
	for _, charset := range testCases {
		_, err := charsetReader(charset, strings.NewReader("test"))
		if err != nil {
			t.Errorf("charsetReader(%q) returned error: %v", charset, err)
		}
	}
}

func TestCharsetReader_UnsupportedCharset(t *testing.T) {
	_, err := charsetReader("nonexistent-charset", strings.NewReader("test"))
	if err == nil {
		t.Error("charsetReader with unsupported charset should return error")
	}
}
