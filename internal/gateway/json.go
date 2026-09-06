package gateway

import "encoding/json"

func jsonBinding(b []byte, v any) error { return json.Unmarshal(b, v) }
