package quota

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusOK        Status = "ok"
	StatusExhausted Status = "exhausted"
	StatusError     Status = "error"
)

type WindowName string

const (
	Window5Hour     WindowName = "5h"
	Window7Day      WindowName = "7d"
	WindowPro       WindowName = "pro"
	WindowFlash     WindowName = "flash"
	WindowFlashLite WindowName = "^lite"
)

func BaseWindow(w WindowName) WindowName {
	base, _, ok := strings.Cut(string(w), ":")
	if !ok || base == "" {
		return w
	}
	return WindowName(base)
}

func WindowBucket(w WindowName) string {
	_, bucket, ok := strings.Cut(string(w), ":")
	if !ok {
		return ""
	}
	return bucket
}

func DisplayWindowLabel(w WindowName) string {
	bucket := WindowBucket(w)
	if bucket == "" {
		return string(w)
	}
	return bucket + " " + string(BaseWindow(w))
}

func IsAggregable(w WindowName) bool {
	_, ok := durationPeriodFor(w)
	return ok
}

func PeriodFor(name WindowName) time.Duration {
	switch BaseWindow(name) {
	case WindowPro, WindowFlash, WindowFlashLite:
		return 24 * time.Hour
	}
	period, _ := durationPeriodFor(name)
	return period
}

// WindowNameForPeriod returns a canonical duration-backed window name.
// Periods use the largest exact whole unit and optionally include a bucket.
func WindowNameForPeriod(periodSeconds int64, bucket string) (WindowName, bool) {
	const maxPeriodSeconds = int64(1<<63-1) / int64(time.Second)
	if periodSeconds <= 0 || periodSeconds > maxPeriodSeconds {
		return "", false
	}

	var base string
	switch {
	case periodSeconds%int64(24*time.Hour/time.Second) == 0:
		base = strconv.FormatInt(periodSeconds/int64(24*time.Hour/time.Second), 10) + "d"
	case periodSeconds%int64(time.Hour/time.Second) == 0:
		base = strconv.FormatInt(periodSeconds/int64(time.Hour/time.Second), 10) + "h"
	case periodSeconds%int64(time.Minute/time.Second) == 0:
		base = strconv.FormatInt(periodSeconds/int64(time.Minute/time.Second), 10) + "m"
	default:
		base = strconv.FormatInt(periodSeconds, 10) + "s"
	}
	return scopedWindow(WindowName(base), bucket), true
}

func durationPeriodFor(name WindowName) (time.Duration, bool) {
	base := string(BaseWindow(name))
	if len(base) < 2 {
		return 0, false
	}

	value, err := strconv.ParseInt(base[:len(base)-1], 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}

	var unitSeconds int64
	switch base[len(base)-1] {
	case 'd':
		unitSeconds = int64(24 * time.Hour / time.Second)
	case 'h':
		unitSeconds = int64(time.Hour / time.Second)
	case 'm':
		unitSeconds = int64(time.Minute / time.Second)
	case 's':
		unitSeconds = 1
	default:
		return 0, false
	}

	const maxPeriodSeconds = int64(1<<63-1) / int64(time.Second)
	if value > maxPeriodSeconds/unitSeconds {
		return 0, false
	}
	periodSeconds := value * unitSeconds
	canonical, ok := WindowNameForPeriod(periodSeconds, "")
	if !ok || BaseWindow(name) != canonical {
		return 0, false
	}
	return time.Duration(periodSeconds) * time.Second, true
}

// OrderedWindows returns fixed window names in canonical display order.
func OrderedWindows() []WindowName {
	return []WindowName{Window5Hour, Window7Day, WindowPro, WindowFlash, WindowFlashLite}
}

// OrderedWindowNames returns the provided windows in canonical display order:
// shared duration windows first, then scoped duration windows grouped by bucket,
// then fixed provider-specific daily windows, then any remaining unknown keys.
func OrderedWindowNames(keys []WindowName) []WindowName {
	present := make(map[WindowName]struct{}, len(keys))
	for _, key := range keys {
		present[key] = struct{}{}
	}

	ordered := make([]WindowName, 0, len(present))
	seen := make(map[WindowName]struct{}, len(present))
	add := func(name WindowName) {
		if _, ok := present[name]; !ok {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		ordered = append(ordered, name)
		seen[name] = struct{}{}
	}

	shared := make([]WindowName, 0, len(present))
	for name := range present {
		if WindowBucket(name) == "" && IsAggregable(name) {
			shared = append(shared, name)
		}
	}
	sort.Slice(shared, func(i, j int) bool {
		pi := PeriodFor(shared[i])
		pj := PeriodFor(shared[j])
		if pi != pj {
			return pi < pj
		}
		return shared[i] < shared[j]
	})
	for _, name := range shared {
		add(name)
	}

	bucketBases := make(map[string]map[WindowName]struct{})
	for name := range present {
		bucket := WindowBucket(name)
		if bucket == "" || !IsAggregable(name) {
			continue
		}
		if bucketBases[bucket] == nil {
			bucketBases[bucket] = make(map[WindowName]struct{})
		}
		bucketBases[bucket][BaseWindow(name)] = struct{}{}
	}
	buckets := make([]string, 0, len(bucketBases))
	for bucket := range bucketBases {
		buckets = append(buckets, bucket)
	}
	sort.Slice(buckets, func(i, j int) bool {
		rank := func(bucket string) int {
			bases := bucketBases[bucket]
			_, has5h := bases[Window5Hour]
			_, has7d := bases[Window7Day]
			switch {
			case len(bases) == 1 && has7d:
				return 0
			case has5h:
				return 1
			default:
				return 2
			}
		}
		ri := rank(buckets[i])
		rj := rank(buckets[j])
		if ri != rj {
			return ri < rj
		}
		return buckets[i] < buckets[j]
	})
	for _, bucket := range buckets {
		bases := make([]WindowName, 0, len(bucketBases[bucket]))
		for base := range bucketBases[bucket] {
			bases = append(bases, base)
		}
		sort.Slice(bases, func(i, j int) bool {
			pi := PeriodFor(bases[i])
			pj := PeriodFor(bases[j])
			if pi != pj {
				return pi < pj
			}
			return bases[i] < bases[j]
		})
		for _, base := range bases {
			add(scopedWindow(base, bucket))
		}
	}

	for _, name := range []WindowName{WindowPro, WindowFlash, WindowFlashLite} {
		add(name)
	}

	remaining := make([]WindowName, 0, len(present)-len(seen))
	for name := range present {
		if _, ok := seen[name]; ok {
			continue
		}
		remaining = append(remaining, name)
	}
	sort.Slice(remaining, func(i, j int) bool {
		ibase := string(BaseWindow(remaining[i]))
		jbase := string(BaseWindow(remaining[j]))
		if ibase != jbase {
			return ibase < jbase
		}
		ibucket := WindowBucket(remaining[i])
		jbucket := WindowBucket(remaining[j])
		if ibucket != jbucket {
			return ibucket < jbucket
		}
		return string(remaining[i]) < string(remaining[j])
	})
	ordered = append(ordered, remaining...)

	return ordered
}

func scopedWindow(base WindowName, bucket string) WindowName {
	if bucket == "" {
		return base
	}
	return WindowName(string(base) + ":" + bucket)
}

// DefaultResetEpoch returns a fallback reset epoch when the API doesn't
// provide one: nowEpoch + periodS (i.e. one full period from now).
func DefaultResetEpoch(periodS, nowEpoch int64) int64 {
	return nowEpoch + periodS
}
