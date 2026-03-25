//go:build windows

package agents

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func TestDecodeCommandOutputUTF16LE(t *testing.T) {
	text := "'ls' 不是内部或外部命令。"
	u16 := utf16.Encode([]rune(text))
	raw := make([]byte, 2+len(u16)*2)
	raw[0] = 0xFF
	raw[1] = 0xFE
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(raw[2+i*2:], v)
	}

	decoded := decodeCommandOutput(raw)
	if decoded != text {
		t.Fatalf("decoded = %q, want %q", decoded, text)
	}
}

func TestDecodeCommandOutputGBK(t *testing.T) {
	text := "'ls' 不是内部或外部命令。"
	raw, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(text))
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, ok := decodeWithCodePage(raw, 936)
	if !ok {
		t.Fatal("expected GBK decode to succeed")
	}
	if strings.TrimSpace(decoded) != text {
		t.Fatalf("decoded = %q, want %q", decoded, text)
	}
}
