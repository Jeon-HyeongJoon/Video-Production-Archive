// Package processprogress frames short tool status lines without buffering arbitrary process output.
package processprogress

type Lines struct {
	OnLine   func(string)
	line     []byte
	overflow bool
}

func (lines *Lines) Write(data []byte) (int, error) {
	for _, b := range data {
		switch {
		case b == '\n' || b == '\r':
			lines.Flush()
		case len(lines.line) < 256:
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
