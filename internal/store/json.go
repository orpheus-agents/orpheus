package store

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// Canonicalize decimal notation without float64 rounding or expanding exponents.
// PostgreSQL jsonb may change 1e3 to 1000 and preserve insignificant zeroes.
func canonicalNumber(n json.Number) json.Number {
	s := strings.ToLower(string(n))
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mantissa, exponent, _ := strings.Cut(s, "e")
	var scale big.Int
	if exponent != "" {
		scale.SetString(exponent, 10)
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "0"
	}
	coefficient := strings.TrimRight(digits, "0")
	scale.Add(&scale, big.NewInt(int64(len(digits)-len(coefficient)-len(fraction))))
	if negative {
		coefficient = "-" + coefficient
	}
	if scale.Sign() != 0 {
		coefficient += "e" + scale.String()
	}
	return json.Number(coefficient)
}

func canonicalValue(v any) any {
	switch value := v.(type) {
	case json.Number:
		return canonicalNumber(value)
	case map[string]any:
		for key, item := range value {
			value[key] = canonicalValue(item)
		}
	case []any:
		for i, item := range value {
			value[i] = canonicalValue(item)
		}
	}
	return v
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(canonicalValue(v))
}

func jsonEqual(a, b json.RawMessage) bool {
	x, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	y, err := canonicalJSON(b)
	return err == nil && bytes.Equal(x, y)
}
