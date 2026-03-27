package workload

func LabelsWithWorkload(base map[string]string, workloadName, resourceKind string) map[string]string {
	out := mergeStringMaps(base, map[string]string{})
	if workloadName != "" {
		out["okdev.io/workload-name"] = workloadName
	}
	if resourceKind != "" {
		out["okdev.io/workload-resource-kind"] = resourceKind
	}
	return out
}

func AnnotationsWithWorkload(base map[string]string, workloadName, apiVersion, resourceKind string) map[string]string {
	out := mergeStringMaps(base, map[string]string{})
	if workloadName != "" {
		out["okdev.io/workload-name"] = workloadName
	}
	if apiVersion != "" {
		out["okdev.io/workload-api-version"] = apiVersion
	}
	if resourceKind != "" {
		out["okdev.io/workload-resource-kind"] = resourceKind
	}
	return out
}
