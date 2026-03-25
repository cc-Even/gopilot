//go:build !windows

package agents

func decodeCommandOutput(raw []byte) string {
	return string(raw)
}
