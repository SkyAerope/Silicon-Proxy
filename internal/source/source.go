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

func ParseAndValidateLines(raw string, withPrefix bool, useRegex bool) []string {
	lines := strings.Split(raw, "\n")
	unique := make(map[string]struct{}, len(lines))

	var linePattern *regexp.Regexp
	if useRegex {
		if withPrefix {
			linePattern = regexp.MustCompile(`(?i)^socks5://(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})$`)
		} else {
			linePattern = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})$`)
		}
	}

	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}

		if useRegex {
			matches := linePattern.FindStringSubmatch(candidate)
			if len(matches) != 3 {
				continue
			}
			candidate = matches[1] + ":" + matches[2]
		} else if withPrefix {
			lower := strings.ToLower(candidate)
			if !strings.HasPrefix(lower, "socks5://") {
				continue
			}
			candidate = strings.TrimSpace(candidate[len("socks5://"):])
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
