// Package processprogress frames short tool status lines without buffering arbitrary process output.
package processprogress

type Lines struct {
	OnLine       func(string)
	MaxLineBytes int // Zero uses the default for numeric progress lines.
	line         []byte
	overflow     bool
}

func (lines *Lines) Write(data []byte) (int, error) {
	limit := lines.MaxLineBytes
	if limit <= 0 {
		limit = 256
	}
	for _, b := range data {
		switch {
		case b == '\n' || b == '\r':
			lines.Flush()
		case len(lines.line) < limit:
			lines.line = append(lines.line, b)
		default:
			lines.overflow = true
		}
	}
	return len(data), nil
}

func (lines *Lines) Flush() {
	if len(lines.line) > 0 && !lines.overflow {
		lines.OnLine(string(lines.line))
	}
	lines.line = lines.line[:0]
	lines.overflow = false
}
