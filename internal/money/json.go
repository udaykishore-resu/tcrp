package money

import "encoding/json"

// jsonUnmarshal is isolated so the rest of the package reads as pure
// arithmetic with no encoding concerns interleaved.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
