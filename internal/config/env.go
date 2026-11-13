package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
)

var validVarNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandEnv substitutes environment variables in the format ${VAR} or ${VAR:-default}.
// Raw '$' characters not enclosed in '${...}' (such as bcrypt hashes) are left untouched.
// Returns an error if a '${' sequence is unclosed or if the variable name is invalid.
func ExpandEnv(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(raw))

	i := 0
	for i < len(raw) {
		if raw[i] == '$' {
			// Check if next char is '{'
			if i+1 < len(raw) && raw[i+1] == '{' {
				// Find closing '}'
				end := bytes.IndexByte(raw[i+2:], '}')
				if end == -1 {
					return nil, fmt.Errorf("unclosed environment variable substitution at index %d: %q", i, string(raw[i:]))
				}
				content := raw[i+2 : i+2+end]

				// Parse ${VAR} or ${VAR:-default}
				varName := ""
				defaultValue := ""
				hasDefault := false

				if sep := bytes.Index(content, []byte(":-")); sep != -1 {
					varName = string(content[:sep])
					defaultValue = string(content[sep+2:])
					hasDefault = true
				} else {
					varName = string(content)
				}

				if varName == "" || !validVarNameRegex.MatchString(varName) {
					return nil, fmt.Errorf("invalid environment variable name %q at index %d", varName, i)
				}

				val, found := os.LookupEnv(varName)
				if hasDefault {
					if found && val != "" {
						buf.WriteString(val)
					} else {
						buf.WriteString(defaultValue)
					}
				} else {
					if found {
						buf.WriteString(val)
					}
					// If unset and no default, resolves to empty string
				}

				i = i + 2 + end + 1
				continue
			}
		}

		buf.WriteByte(raw[i])
		i++
	}

	return buf.Bytes(), nil
}
