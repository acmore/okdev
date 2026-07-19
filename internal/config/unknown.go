package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// removedSpecFields maps once-valid spec paths to a pointer at what replaced
// them, so a leftover field warns with history instead of a bare "unknown".
var removedSpecFields = map[string]string{
	"session.ttlHours":           "removed — nothing enforced it; sessions no longer expire via okdev",
	"session.idleTimeoutMinutes": "removed — nothing enforced it; sessions no longer expire via okdev",
}

// UnknownSpecFieldWarnings reports config keys under spec that okdev does not
// recognize. Config unmarshalling is non-strict, so a typo like
// `spec.context` (the real field is `spec.kubeContext`) is silently ignored
// and costs real debugging time (#172). Two levels are checked — spec itself
// and each spec section whose type lives in this package; sections carrying
// external Kubernetes types (podTemplate, volumes, ...) stay opaque.
func UnknownSpecFieldWarnings(raw []byte) []string {
	var payload map[string]any
	if err := yaml.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	specMap, ok := payload["spec"].(map[string]any)
	if !ok {
		return nil
	}
	specType := reflect.TypeOf(DevEnvSpec{})
	known := yamlFieldsByName(specType)

	var warnings []string
	for key, value := range specMap {
		field, ok := known[key]
		if !ok {
			warnings = append(warnings, unknownFieldWarning(key, "", knownKeys(known)))
			continue
		}
		sectionType := field.Type
		if sectionType.Kind() == reflect.Ptr {
			sectionType = sectionType.Elem()
		}
		if sectionType.Kind() != reflect.Struct || sectionType.PkgPath() != specType.PkgPath() {
			continue // external or scalar type: opaque
		}
		sectionMap, ok := value.(map[string]any)
		if !ok {
			continue
		}
		sectionKnown := yamlFieldsByName(sectionType)
		for subKey := range sectionMap {
			if _, ok := sectionKnown[subKey]; !ok {
				warnings = append(warnings, unknownFieldWarning(subKey, key, knownKeys(sectionKnown)))
			}
		}
	}
	sort.Strings(warnings)
	return warnings
}

func unknownFieldWarning(key, section string, known []string) string {
	path := key
	if section != "" {
		path = section + "." + key
	}
	if hint, ok := removedSpecFields[path]; ok {
		return fmt.Sprintf("spec.%s is ignored: %s", path, hint)
	}
	return fmt.Sprintf("spec.%s is not a recognized field and is ignored (known: %s)", path, strings.Join(known, ", "))
}

func yamlFieldsByName(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		out[tag] = field
	}
	return out
}

func knownKeys(fields map[string]reflect.StructField) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
