package sync

import (
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/shellutil"
)

type Pair struct {
	Local  string
	Remote string
	// Direction is the resolved sync direction for this mapping's folder
	// (bi/up/down, never empty; see config.SyncPathSpec).
	Direction string
}

func ParsePairs(configured []config.SyncPathSpec, defaultRemote string) ([]Pair, error) {
	if len(configured) == 0 {
		return []Pair{{Local: ".", Remote: defaultRemote, Direction: config.SyncDirectionBi}}, nil
	}
	out := make([]Pair, 0, len(configured))
	for _, item := range configured {
		local := strings.TrimSpace(item.Local)
		remote := strings.TrimSpace(item.Remote)
		if local == "" || remote == "" {
			return nil, fmt.Errorf("invalid sync path mapping %q, expected local:remote", item.Local+":"+item.Remote)
		}
		out = append(out, Pair{Local: local, Remote: remote, Direction: item.EffectiveDirection()})
	}
	return out, nil
}

func ShellEscape(s string) string {
	return shellutil.Quote(s)
}
