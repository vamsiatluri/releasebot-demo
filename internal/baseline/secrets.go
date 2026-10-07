package baseline

import "strings"

// secretish are substrings that mark an environment variable as something whose
// VALUE must never leave the source account through this tool.
//
// The list is deliberately broad and the classifier deliberately over-redacts.
// A redacted value that turns out to be harmless costs someone ten seconds; a
// captured credential written to a JSON file that then gets attached to a ticket
// is an incident. There is no symmetry between those two errors, so the default
// is not symmetric either.
var secretish = []string{
	"token", "secret", "password", "passwd", "credential", "cred",
	"key", "apikey", "api_key", "auth", "signing", "private",
	"session", "cert", "certificate", "pfx", "pem", "salt", "hash",
	"dsn", "connection_string", "conn_str", "webhook",
}

// nonSecretExact are keys that contain a secretish substring but are known not
// to carry a credential. Each one is an explicit, reviewable exception rather
// than a clever rule -- "KEY" matching "MONKEY" is the kind of thing a regex
// gets wrong silently.
var nonSecretExact = map[string]bool{
	"AWS_REGION":                true,
	"AWS_DEFAULT_REGION":        true,
	"AWS_LAMBDA_FUNCTION_NAME":  true,
	"SECRETS_PREFIX":            true, // a prefix is a pointer, not a value
	"SECRET_ARN":                true, // likewise: an ARN names a secret
	"KEY_PREFIX":                true,
	"AUTH_MODE":                 true,
	"SIGNING_ALGORITHM":         true,
}

// IsSecretish reports whether a variable's VALUE should be withheld.
func IsSecretish(key string) bool {
	if nonSecretExact[strings.ToUpper(key)] {
		return false
	}
	// An ARN naming a secret is a reference, not the secret. Capturing it is the
	// whole point of the "hold a reference, not a value" design.
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "_arn") || strings.HasSuffix(k, "arn") {
		return false
	}
	for _, s := range secretish {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// SplitEnv separates an environment map into the keys whose values are safe to
// record and the keys whose values are not. Values for the second group are
// never returned, only the names.
func SplitEnv(env map[string]string) (keys []string, safe map[string]string, secretLooking []string) {
	safe = map[string]string{}
	for k, v := range env {
		keys = append(keys, k)
		if IsSecretish(k) {
			secretLooking = append(secretLooking, k)
			continue
		}
		safe[k] = v
	}
	sortStrings(keys)
	sortStrings(secretLooking)
	return keys, safe, secretLooking
}
