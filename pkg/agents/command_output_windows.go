//go:build windows

package agents

import (
	"bytes"
	"encoding/binary"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

var (
	kernel32DLL  = windows.NewLazySystemDLL("kernel32.dll")
	getOEMCPProc = kernel32DLL.NewProc("GetOEMCP")
)

func decodeCommandOutput(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(raw) {
		return string(raw)
	}

	if decoded, ok := decodeUTF16Output(raw); ok {
		return decoded
	}

	for _, codePage := range windowsCodePages() {
		decoded, ok := decodeWithCodePage(raw, codePage)
		if ok {
			return decoded
		}
	}

	return string(raw)
}

func windowsCodePages() []uint32 {
	codePages := []uint32{windows.GetACP(), getOEMCP()}
	seen := make(map[uint32]struct{}, len(codePages))
	out := make([]uint32, 0, len(codePages))
	for _, cp := range codePages {
		if cp == 0 {
			continue
		}
		if _, ok := seen[cp]; ok {
			continue
		}
		seen[cp] = struct{}{}
		out = append(out, cp)
	}
	return out
}

func getOEMCP() uint32 {
	if err := kernel32DLL.Load(); err != nil {
		return 0
	}
	r0, _, _ := syscall.SyscallN(getOEMCPProc.Addr())
	return uint32(r0)
}

func decodeUTF16Output(raw []byte) (string, bool) {
	if len(raw) < 2 || len(raw)%2 != 0 {
		return "", false
	}

	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) {
		return decodeUTF16(raw[2:], binary.LittleEndian), true
	}
	if bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) {
		return decodeUTF16(raw[2:], binary.BigEndian), true
	}
	if !looksLikeUTF16(raw) {
		return "", false
	}
	return decodeUTF16(raw, binary.LittleEndian), true
}

func looksLikeUTF16(raw []byte) bool {
	if len(raw) < 4 || len(raw)%2 != 0 {
		return false
	}

	nulls := 0
	for i := 1; i < len(raw); i += 2 {
		if raw[i] == 0 {
			nulls++
		}
	}
	return nulls*2 >= len(raw)/2
}

func decodeUTF16(raw []byte, order binary.ByteOrder) string {
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u16 = append(u16, order.Uint16(raw[i:i+2]))
	}
	return strings.TrimRight(string(utf16.Decode(u16)), "\x00")
}

func decodeWithCodePage(raw []byte, codePage uint32) (string, bool) {
	enc := encodingForCodePage(codePage)
	if enc == nil {
		return "", false
	}
	decoded, _, err := transform.Bytes(enc.NewDecoder(), raw)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func encodingForCodePage(codePage uint32) encoding.Encoding {
	switch codePage {
	case 437:
		return charmap.CodePage437
	case 850:
		return charmap.CodePage850
	case 852:
		return charmap.CodePage852
	case 855:
		return charmap.CodePage855
	case 866:
		return charmap.CodePage866
	case 874:
		return charmap.Windows874
	case 932:
		return japanese.ShiftJIS
	case 936:
		return simplifiedchinese.GBK
	case 949:
		return korean.EUCKR
	case 950:
		return traditionalchinese.Big5
	case 1250:
		return charmap.Windows1250
	case 1251:
		return charmap.Windows1251
	case 1252:
		return charmap.Windows1252
	case 1253:
		return charmap.Windows1253
	case 1254:
		return charmap.Windows1254
	case 1255:
		return charmap.Windows1255
	case 1256:
		return charmap.Windows1256
	case 1257:
		return charmap.Windows1257
	case 1258:
		return charmap.Windows1258
	default:
		return nil
	}
}
