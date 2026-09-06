package matching

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Script is a market config plus a command sequence: the golden-file and
// `exchangectl replay` format. On disk it is JSON lines: the first line is
// {"market": {...}}, every following line is one Command.
type Script struct {
	Market   MarketConfig
	Commands []Command
}

type scriptLine struct {
	Market *MarketConfig `json:"market,omitempty"`
	Command
}

// ReadScript parses the JSON-lines script format.
func ReadScript(r io.Reader) (Script, error) {
	var s Script
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var line scriptLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return Script{}, fmt.Errorf("script line %d: %w", lineNo, err)
		}
		switch {
		case line.Market != nil:
			if lineNo != 1 && len(s.Commands) > 0 {
				return Script{}, fmt.Errorf("script line %d: market header must come first", lineNo)
			}
			s.Market = *line.Market
		case line.New != nil || line.Cancel != nil:
			s.Commands = append(s.Commands, line.Command)
		default:
			return Script{}, fmt.Errorf("script line %d: neither market nor command", lineNo)
		}
	}
	if err := sc.Err(); err != nil {
		return Script{}, err
	}
	if s.Market.Symbol == "" {
		return Script{}, fmt.Errorf("script: missing market header line")
	}
	return s, nil
}

// WriteScript writes a Script in the JSON-lines format ReadScript reads.
func WriteScript(w io.Writer, s Script) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(scriptLine{Market: &s.Market}); err != nil {
		return err
	}
	for _, c := range s.Commands {
		if err := enc.Encode(scriptLine{Command: c}); err != nil {
			return err
		}
	}
	return nil
}

// Run applies every command of the script to a fresh book and returns the
// events per command and the final book.
func Run(s Script) ([][]Event, *Book, error) {
	book, err := New(s.Market)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]Event, 0, len(s.Commands))
	for i, c := range s.Commands {
		evs, err := book.Apply(c)
		if err != nil {
			return out, book, fmt.Errorf("command %d (seq %d): %w", i+1, c.Seq, err)
		}
		out = append(out, evs)
	}
	return out, book, nil
}
