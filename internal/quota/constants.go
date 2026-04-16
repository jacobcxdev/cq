package quota

import (
	"sort"
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
	switch BaseWindow(w) {
	case Window5Hour, Window7Day:
		return true
	default:
		return false
	}
}

func PeriodFor(name WindowName) time.Duration {
	switch BaseWindow(name) {
	case Window5Hour:
		return 5 * time.Hour
	case Window7Day:
		return 7 * 24 * time.Hour
	case WindowPro, WindowFlash, WindowFlashLite:
		return 24 * time.Hour
	default:
		return 0
	}
}

// OrderedWindows returns fixed window names in canonical display order.
func OrderedWindows() []WindowName {
	return []WindowName{Window5Hour, Window7Day, WindowPro, WindowFlash, WindowFlashLite}
}

// OrderedWindowNames returns the provided windows in canonical display order:
// shared windows first, then bucket-scoped 5h/7d windows grouped by bucket,
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

	shared := []WindowName{Window5Hour, Window7Day}
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
			case has7d && !has5h:
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
		for _, base := range shared {
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
