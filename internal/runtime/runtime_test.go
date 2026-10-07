package runtime

import "testing"

func TestInvocationAlias(t *testing.T) {
	cases := map[string]string{
		"arn:aws:lambda:us-east-1:123456789012:function:cutRelease:prod":    "prod",
		"arn:aws:lambda:us-east-1:123456789012:function:cutRelease:test":    "test",
		"arn:aws:lambda:us-east-1:123456789012:function:cutRelease:41":      "41",
		"arn:aws:lambda:us-east-1:123456789012:function:cutRelease:$LATEST": "",
		"arn:aws:lambda:us-east-1:123456789012:function:cutRelease":         "",
		"":                                                                  "",
	}
	for arn, want := range cases {
		if got := (Invocation{InvokedFunctionARN: arn}).Alias(); got != want {
			t.Errorf("Alias(%q) = %q, want %q", arn, got, want)
		}
	}
}
