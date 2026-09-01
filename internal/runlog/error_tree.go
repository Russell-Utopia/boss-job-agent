package runlog

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxErrorNodes        = 64
	maxErrorDepth        = 32
	maxErrorMessageBytes = 2048
)

var (
	sensitiveAssignmentPattern = regexp.MustCompile(`(?i)(authorization|proxy-authorization|set-cookie|cookie|resume|jd|greeting|prompt|model_output|model output)["']?\s*[:=]\s*[^\r\n]*`)
)

type errorNode struct {
	Path             string `json:"path"`
	Type             string `json:"type"`
	Message          string `json:"message"`
	MessageTruncated bool   `json:"message_truncated,omitempty"`
}

func snapshotErrorTree(root error, redactedValues ...string) ([]errorNode, bool) {
	nodes := make([]errorNode, 0, 4)
	truncated := false
	var visit func(error, string, int)
	visit = func(current error, path string, depth int) {
		if current == nil {
			return
		}
		if len(nodes) >= maxErrorNodes || depth >= maxErrorDepth {
			truncated = true
			return
		}
		message, messageTruncated := sanitizeErrorMessage(current.Error(), redactedValues)
		nodes = append(nodes, errorNode{
			Path:             path,
			Type:             fmt.Sprintf("%T", current),
			Message:          message,
			MessageTruncated: messageTruncated,
		})
		if multiple, ok := current.(interface{ Unwrap() []error }); ok {
			for index, child := range multiple.Unwrap() {
				visit(child, fmt.Sprintf("%s.%d", path, index), depth+1)
			}
			return
		}
		if single, ok := current.(interface{ Unwrap() error }); ok {
			visit(single.Unwrap(), path+".0", depth+1)
		}
	}
	visit(root, "0", 0)
	return nodes, truncated
}

func sanitizeErrorMessage(message string, redactedValues []string) (string, bool) {
	for _, value := range redactedValues {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	if match := sensitiveAssignmentPattern.FindStringSubmatchIndex(message); match != nil {
		message = message[:match[0]] + message[match[2]:match[3]] + "=[REDACTED]"
	}
	if containsHTMLDocument(message) {
		message = message[:strings.Index(message, "<")] + "[REDACTED HTML]"
	}
	if len(message) <= maxErrorMessageBytes {
		return message, false
	}
	truncated := message[:maxErrorMessageBytes]
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated, true
}

func containsHTMLDocument(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype")
}
