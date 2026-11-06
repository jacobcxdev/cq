package dots

import "strings"

// rspackBody scans delimiters without executing renderer code.
func rspackBody(source string, start int) (string, bool) {
	if start < 0 || start >= len(source) || source[start] != '{' {
		return "", false
	}
	end, ok := rspackBalanced(source, start)
	if !ok {
		return "", false
	}
	return source[start:end], true
}
func rspackBalanced(s string, start int) (int, bool) {
	return rspackBalancedDepth(s, start, 0)
}

func rspackBalancedDepth(s string, start, nesting int) (int, bool) {
	if nesting >= 128 || start < 0 || start >= len(s) || s[start] != '{' {
		return 0, false
	}
	depth := 0
	regexAllowed := true
	for i := start; i < len(s); {
		c := s[i]
		if c == '\'' || c == '"' {
			q := c
			i++
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == q {
					i++
					break
				}
				i++
			}
			regexAllowed = false
			continue
		}
		if c == '`' {
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '`' {
					i++
					closed = true
					break
				}
				if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
					var ok bool
					i, ok = rspackBalancedDepth(s, i+1, nesting+1)
					if !ok {
						return 0, false
					}
					continue
				}
				i++
			}
			if !closed {
				return 0, false
			}
			regexAllowed = false
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				i += 2
				for i < len(s) && s[i] != '\n' {
					i++
				}
				continue
			}
			if s[i+1] == '*' {
				j := strings.Index(s[i+2:], "*/")
				if j < 0 {
					return 0, false
				}
				i += j + 4
				continue
			}
			if regexAllowed {
				i++
				class := false
				closed := false
				for i < len(s) {
					if s[i] == '\\' {
						i += 2
						continue
					}
					if s[i] == '[' {
						class = true
					}
					if s[i] == ']' {
						class = false
					}
					if s[i] == '/' && !class {
						i++
						closed = true
						break
					}
					i++
				}
				if !closed {
					return 0, false
				}
				for i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
					i++
				}
				regexAllowed = false
				continue
			}
		}
		if c == '{' {
			depth++
			regexAllowed = true
			i++
			continue
		}
		if c == '}' {
			depth--
			i++
			if depth == 0 {
				return i, true
			}
			regexAllowed = false
			continue
		}
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			i++
			continue
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' {
			j := i + 1
			for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9' || s[j] == '_' || s[j] == '$') {
				j++
			}
			word := s[i:j]
			regexAllowed = word == "return" || word == "throw" || word == "case" || word == "typeof" || word == "void" || word == "delete" || word == "new"
			i = j
			continue
		}
		regexAllowed = strings.ContainsRune("=(:,[!&|?;+-*%~<>^", rune(c))
		i++
	}
	return 0, false
}
