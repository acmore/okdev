package sync

import (
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/shellutil"
)

type Pair struct {
	Local  string
	Remote string
}

func ParsePairs(configured []string, defaultRemote string) ([]Pair, error) {
	if len(configured) == 0 {
		return []Pair{{Local: ".", Remote: defaultRemote}}, nil
	}
	out := make([]Pair, 0, len(configured))
	for _, item := range configured {
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid sync path mapping %q, expected local:remote", item)
		}
		out = append(out, Pair{Local: strings.TrimSpace(parts[0]), Remote: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

func ShellEscape(s string) string {
	return shellutil.Quote(s)
}
