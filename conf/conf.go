package conf

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig   LogConfig    `mapstructure:"Log"`
	NodeConfigs []NodeConfig `mapstructure:"Nodes"`
	PprofPort   int          `mapstructure:"PprofPort"`
}

type LogConfig struct {
	Level      string `mapstructure:"Level"`
	Output     string `mapstructure:"Output"`
	Access     string `mapstructure:"Access"`
	SNTPAccess bool   `mapstructure:"SNTPAccess"`
}

type NodeConfig struct {
	APIHost                 string `mapstructure:"ApiHost"`
	NodeID                  int    `mapstructure:"NodeID"`
	Key                     string `mapstructure:"ApiKey"`
	Timeout                 int    `mapstructure:"Timeout"`
	RetryCount              *int   `mapstructure:"RetryCount"`
	AppTransportTokenSecret string `mapstructure:"AppTransportTokenSecret"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:      "warning",
			Output:     "",
			Access:     "none",
			SNTPAccess: true,
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	p.LogConfig.Normalize()
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	return nil
}

func (p *LogConfig) Normalize() {
	p.Level = strings.ToLower(strings.TrimSpace(p.Level))
	if p.Level == "" {
		p.Level = "warning"
	}
	p.Output = strings.TrimSpace(p.Output)
	p.Access = strings.TrimSpace(p.Access)
	if p.Access == "" {
		p.Access = "none"
	}
}

func (p LogConfig) CoreAccessLog() string {
	switch strings.ToLower(strings.TrimSpace(p.Access)) {
	case "", "none":
		return "none"
	case "console":
		return ""
	default:
		return p.Access
	}
}

func NormalizeLogConfigFile(filePath string) (bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}
	normalized, changed, err := normalizeLogConfigBytes(data)
	if err != nil || !changed {
		return changed, err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(filePath, normalized, info.Mode())
}

func normalizeLogConfigBytes(data []byte) ([]byte, bool, error) {
	start, end, err := findTopLevelObjectField(data, "Log")
	if err != nil || start < 0 {
		return data, false, err
	}

	var fields map[string]stdjson.RawMessage
	if err := stdjson.Unmarshal(data[start:end], &fields); err != nil {
		return nil, false, fmt.Errorf("decode Log config: %w", err)
	}

	delete(fields, "Level")
	delete(fields, "Output")
	delete(fields, "Access")

	logIndent, fieldIndent := logObjectIndents(data, start)
	normalizedLog := buildNormalizedLogObject(logIndent, fieldIndent, fields)
	normalized := make([]byte, 0, len(data)-end+start+len(normalizedLog))
	normalized = append(normalized, data[:start]...)
	normalized = append(normalized, normalizedLog...)
	normalized = append(normalized, data[end:]...)

	if bytes.Equal(data, normalized) {
		return data, false, nil
	}
	return normalized, true, nil
}

func buildNormalizedLogObject(logIndent, fieldIndent []byte, extra map[string]stdjson.RawMessage) []byte {
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteString("{\n")
	writeLogField(&b, fieldIndent, "Level", `"warning"`, true)
	writeLogField(&b, fieldIndent, "Output", `""`, true)
	writeLogField(&b, fieldIndent, "Access", `"none"`, len(keys) > 0)
	for i, key := range keys {
		writeLogField(&b, fieldIndent, key, string(extra[key]), i+1 < len(keys))
	}
	b.Write(logIndent)
	b.WriteByte('}')
	return b.Bytes()
}

func writeLogField(b *bytes.Buffer, indent []byte, key string, value string, comma bool) {
	b.Write(indent)
	b.WriteString(strconv.Quote(key))
	b.WriteString(": ")
	b.WriteString(value)
	if comma {
		b.WriteByte(',')
	}
	b.WriteByte('\n')
}

func logObjectIndents(data []byte, objectStart int) ([]byte, []byte) {
	lineStart := objectStart
	for lineStart > 0 && data[lineStart-1] != '\n' && data[lineStart-1] != '\r' {
		lineStart--
	}
	logIndentEnd := lineStart
	for logIndentEnd < len(data) && (data[logIndentEnd] == ' ' || data[logIndentEnd] == '\t') {
		logIndentEnd++
	}
	logIndent := append([]byte(nil), data[lineStart:logIndentEnd]...)

	fieldIndent := append([]byte(nil), logIndent...)
	fieldIndent = append(fieldIndent, ' ', ' ', ' ', ' ')
	for i := objectStart + 1; i < len(data); {
		if data[i] == '\r' || data[i] == '\n' {
			for i < len(data) && (data[i] == '\r' || data[i] == '\n') {
				i++
			}
			start := i
			for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
				i++
			}
			if i < len(data) && data[i] == '"' {
				return logIndent, append([]byte(nil), data[start:i]...)
			}
			continue
		}
		i++
	}
	return logIndent, fieldIndent
}

func findTopLevelObjectField(data []byte, field string) (int, int, error) {
	rootStart := skipSpace(data, 0)
	if rootStart >= len(data) || data[rootStart] != '{' {
		return -1, -1, fmt.Errorf("config root is not a JSON object")
	}

	for i := rootStart + 1; i < len(data); {
		i = skipSpaceAndCommas(data, i)
		if i >= len(data) || data[i] == '}' {
			return -1, -1, nil
		}
		if data[i] != '"' {
			return -1, -1, fmt.Errorf("invalid JSON object key near byte %d", i)
		}
		keyStart := i
		keyEnd, err := scanJSONString(data, keyStart)
		if err != nil {
			return -1, -1, err
		}
		var key string
		if err := stdjson.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return -1, -1, err
		}
		i = skipSpace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return -1, -1, fmt.Errorf("missing colon after key %q", key)
		}
		valueStart := skipSpace(data, i+1)
		valueEnd, err := scanJSONValue(data, valueStart)
		if err != nil {
			return -1, -1, err
		}
		if key == field {
			if valueStart >= len(data) || data[valueStart] != '{' {
				return -1, -1, fmt.Errorf("%q is not a JSON object", field)
			}
			return valueStart, valueEnd, nil
		}
		i = valueEnd
	}
	return -1, -1, nil
}

func scanJSONValue(data []byte, start int) (int, error) {
	if start >= len(data) {
		return 0, fmt.Errorf("missing JSON value")
	}
	switch data[start] {
	case '"':
		return scanJSONString(data, start)
	case '{', '[':
		return scanJSONContainer(data, start)
	default:
		i := start
		for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' {
			i++
		}
		return i, nil
	}
}

func scanJSONContainer(data []byte, start int) (int, error) {
	open := data[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	for i := start; i < len(data); i++ {
		switch data[i] {
		case '"':
			end, err := scanJSONString(data, i)
			if err != nil {
				return 0, err
			}
			i = end - 1
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated JSON container")
}

func scanJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, fmt.Errorf("missing JSON string")
	}
	escaped := false
	for i := start + 1; i < len(data); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch data[i] {
		case '\\':
			escaped = true
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func skipSpace(data []byte, start int) int {
	for start < len(data) && unicode.IsSpace(rune(data[start])) {
		start++
	}
	return start
}

func skipSpaceAndCommas(data []byte, start int) int {
	for start < len(data) && (unicode.IsSpace(rune(data[start])) || data[start] == ',') {
		start++
	}
	return start
}

func intPtr(v int) *int {
	return &v
}
