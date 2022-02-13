package eql

import (
	"fmt"
	"strings"
)

type state int

const (
	stateKey state = iota
	stateValue
	stateQuote
)

func Parse(str string) (map[string]string, error) {
	var s state
	isEscaping := false
	quoteStyle := ' '
	var tmpKey, tmpValue []rune
	result := map[string]string{}

	pushAndReset := func() {
		k := strings.TrimSpace(string(tmpKey))
		v := strings.TrimSpace(string(tmpValue))
		result[k] = v
		tmpKey = tmpKey[:0]
		tmpValue = tmpValue[:0]
		isEscaping = false
		s = stateKey
	}

	for i, r := range str {
		switch s {
		case stateKey:
			if r == ' ' && len(tmpKey) == 0 {
				continue
			} else if r == '=' {
				if len(tmpKey) == 0 {
					return nil, fmt.Errorf("unexpected '=' at position %d", i+1)
				}
				s = stateValue
				continue
			}

			tmpKey = append(tmpKey, r)
		case stateValue:
			if len(tmpValue) == 0 && r == ' ' {
				continue
			}

			if r == '"' || r == '\'' && len(tmpValue) == 0 {
				s = stateQuote
				quoteStyle = r
				continue
			}

			if r == ' ' {
				pushAndReset()
				continue
			}

			tmpValue = append(tmpValue, r)

		case stateQuote:
			if r == '\\' {
				isEscaping = true
				continue
			} else if isEscaping {
				if r != quoteStyle {
					tmpValue = append(tmpValue, '\\')
				}
				isEscaping = false
			} else if !isEscaping && r == quoteStyle {
				pushAndReset()
				continue
			}

			tmpValue = append(tmpValue, r)
		}
	}

	if s == stateQuote {
		return nil, fmt.Errorf("unterminated quoted string")
	}

	if s == stateValue {
		pushAndReset()
	}

	return result, nil
}
