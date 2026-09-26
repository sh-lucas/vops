package compose

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// interpolateNode replaces ${VAR} forms in every scalar value (not keys).
func interpolateNode(n *yaml.Node, env map[string]string, warns *[]string) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := interpolateNode(n.Content[i], env, warns); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			if err := interpolateNode(c, env, warns); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if !strings.Contains(n.Value, "$") {
			return nil
		}
		v, err := Interpolate(n.Value, env, warns)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		n.Value = v
		if n.Style == 0 {
			n.Tag = "" // let "${PORT}" become an int again
		}
	}
	return nil
}

// Interpolate expands $VAR, ${VAR}, ${VAR:-def}, ${VAR-def}, ${VAR:?err}, ${VAR?err}, ${VAR:+alt}, ${VAR+alt} and $$.
func Interpolate(s string, env map[string]string, warns *[]string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 == len(s) {
			out.WriteByte(c)
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			out.WriteByte('$')
			i++
		case next == '{':
			end := matchBrace(s, i+1)
			if end < 0 {
				return "", fmt.Errorf("unclosed ${ in %q", s)
			}
			v, err := expand(s[i+2:end], env, warns)
			if err != nil {
				return "", err
			}
			out.WriteString(v)
			i = end
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			out.WriteString(lookup(s[i+1:j], env, warns))
			i = j - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}

func matchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isNameChar(c byte) bool  { return isNameStart(c) || c >= '0' && c <= '9' }

func lookup(name string, env map[string]string, warns *[]string) string {
	v, ok := env[name]
	if !ok {
		w := fmt.Sprintf("variable %s is not set, using an empty string", name)
		if !slices.Contains(*warns, w) {
			*warns = append(*warns, w)
		}
	}
	return v
}

func expand(expr string, env map[string]string, warns *[]string) (string, error) {
	j := 0
	for j < len(expr) && isNameChar(expr[j]) {
		j++
	}
	name, op := expr[:j], expr[j:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("invalid variable ${%s}", expr)
	}
	v, set := env[name]
	if op == "" {
		return lookup(name, env, warns), nil
	}
	var word string
	colon := strings.HasPrefix(op, ":")
	if colon {
		op = op[1:]
	}
	if op == "" {
		return "", fmt.Errorf("invalid variable ${%s}", expr)
	}
	word = op[1:]
	present := set && (!colon || v != "")
	switch op[0] {
	case '-':
		if present {
			return v, nil
		}
		return Interpolate(word, env, warns)
	case '+':
		if present {
			return Interpolate(word, env, warns)
		}
		return "", nil
	case '?':
		if present {
			return v, nil
		}
		msg, _ := Interpolate(word, env, warns)
		if msg == "" {
			msg = "is required"
		}
		return "", fmt.Errorf("variable %s: %s (set it with: vops env set <project> %s=...)", name, msg, name)
	}
	return "", fmt.Errorf("invalid variable ${%s}", expr)
}
