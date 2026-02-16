package source

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

type Source interface {
	ID() string
	Interval() time.Duration
	Fetch() ([]string, error)
}

func ParseAndValidateLines(raw string, withPrefix bool) []string {
	lines := strings.Split(raw, "\n")
	unique := make(map[string]struct{}, len(lines))

	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}

		if withPrefix {
			if !strings.HasPrefix(strings.ToLower(candidate), "socks5://") {
				continue
			}
			candidate = strings.TrimPrefix(strings.TrimPrefix(candidate, "socks5://"), "SOCKS5://")
		}

		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}

		if !isValidProxyAddress(candidate) {
			continue
		}

		unique[candidate] = struct{}{}
	}

	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}

	return result
}

func isValidProxyAddress(value string) bool {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return false
	}

	if host == "" {
		return false
	}

	if parsed := net.ParseIP(host); parsed != nil {
		if parsed.IsUnspecified() || parsed.IsLoopback() || parsed.IsMulticast() {
			return false
		}
		if parsed.IsPrivate() || parsed.IsLinkLocalMulticast() || parsed.IsLinkLocalUnicast() {
			return false
		}
		return true
	}

	return domainPattern.MatchString(host)
}

func buildSourceID(sourceType, value string) string {
	return fmt.Sprintf("%s:%s", sourceType, value)
}
