package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bytes is a size written the way an operator writes one: "25MiB", "10MB", or a plain number
// of bytes.
type Bytes int64

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	{"B", 1},
}

func (b *Bytes) UnmarshalJSON(data []byte) error {
	// A bare number is bytes; a string may carry a unit.
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = Bytes(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("a size is a number of bytes or a string like \"25MiB\"")
	}
	v, err := ParseBytes(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func (b Bytes) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

// ParseBytes reads "25MiB" and friends.
func ParseBytes(s string) (Bytes, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	for _, u := range byteUnits {
		if rest, ok := strings.CutSuffix(t, u.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return 0, fmt.Errorf("%q is not a size", s)
			}
			return Bytes(n * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	return Bytes(n), nil
}

func (b Bytes) String() string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return strconv.FormatInt(int64(b)/(1<<30), 10) + "GiB"
	case b >= 1<<20 && b%(1<<20) == 0:
		return strconv.FormatInt(int64(b)/(1<<20), 10) + "MiB"
	case b >= 1<<10 && b%(1<<10) == 0:
		return strconv.FormatInt(int64(b)/(1<<10), 10) + "KiB"
	default:
		return strconv.FormatInt(int64(b), 10) + "B"
	}
}

// Duration is a Go duration string in JSON: "60s", "2m".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("a duration is a string like \"60s\"")
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d Duration) String() string { return time.Duration(d).String() }

// D is the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Recipients is a channel's pinned destination: one address or several.
//
// Written either way, because a config file is hand-written and one recipient is the common
// case: "ops@example.com" and ["ops@example.com", "oncall@example.com"] both work.
type Recipients []string

func (r *Recipients) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			return fmt.Errorf("an empty recipient")
		}
		*r = Recipients{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("a recipient is a string or an array of strings")
	}
	for _, v := range many {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("an empty recipient")
		}
	}
	*r = Recipients(many)
	return nil
}
