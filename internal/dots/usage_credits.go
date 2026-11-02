package dots

import "github.com/jacobcxdev/cq/internal/codexusage"

func floorNativeUsageCredits(usage map[string]any) bool {
	return codexusage.FloorCredits(usage)
}

func floorNativeUsageCreditResponse(data []byte) []byte {
	return codexusage.FloorCreditResponse(data)
}

func floorNativeUsageCreditEvent(frame map[string]any) bool {
	return codexusage.FloorCreditEvent(frame)
}
