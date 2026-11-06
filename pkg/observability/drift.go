package observability

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Reports whether a row's field set matches a current or legacy contract shape, values aside
func CounterDimensionsShapeIsKnown(metricName string, dimensions map[string]any) bool {
	fields := keysOf(dimensions)
	if current, ok := metricFields[metricName]; ok && fields.Equal(current) {
		return true
	}
	for _, legacy := range legacyMetricFields[metricName] {
		if fields.Equal(legacy) {
			return true
		}
	}
	return false
}

// KeyError is the dict lookup failure for a metric or field the contract does not list
type KeyError struct{ Key string }

func (e KeyError) Error() string { return pyrepr.Repr(e.Key) }

// Rebuckets out of contract enum values into the field's custom, other or unknown fallback, nil when one cannot be
func ConformCounterDimensions(metricName string, dimensions map[string]any) (map[string]any, error) {
	conformed := make(map[string]any, len(dimensions))
	for field, value := range dimensions {
		conformed[field] = value
	}
	identifiers := identifierFields[metricName]
	contract, ok := counterDimensionValues[metricName]
	if !ok {
		return nil, KeyError{Key: metricName}
	}
	for field, value := range dimensions {
		text, isText := value.(string)
		if !isText {
			return nil, nil
		}
		if maxLength, isIdentifier := identifiers[field]; isIdentifier {
			if text != metricIdentifier(text, maxLength) {
				return nil, nil
			}
			continue
		}
		allowed, known := contract[field]
		if !known {
			return nil, KeyError{Key: field}
		}
		if allowed.Has(text) {
			continue
		}
		fallback := ""
		for _, candidate := range []string{"custom", "other", "unknown"} {
			if allowed.Has(candidate) {
				fallback = candidate
				break
			}
		}
		if fallback == "" {
			return nil, nil
		}
		conformed[field] = fallback
	}
	return conformed, nil
}
