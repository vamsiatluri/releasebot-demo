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
	"AWS_REGION":               true,
	"AWS_DEFAULT_REGION":       true,
	"AWS_LAMBDA_FUNCTION_NAME": true,
	"SECRETS_PREFIX":           true, // a prefix is a pointer, not a value
	"SECRET_ARN":               true, // likewise: an ARN names a secret
	// ⚠️ A rotation marker: a timestamp recorded so a config-only republish
	// forces a cold start. Named SECRET_GENERATION originally, which the
	// classifier read as a credential and reported as plaintext -- the tool
	// flagging the hardening's own bookkeeping as the thing it was fixing.
	"CONFIG_GENERATION": true,
	"KEY_PREFIX":        true,
	"AUTH_MODE":         true,
	"SIGNING_ALGORITHM": true,
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
// SplitEnv classifies a function's environment.
//
// secretLooking = a secret-looking NAME holding a LITERAL. That is the finding.
// byReference   = a secret-looking NAME holding a POINTER. That is the FIX, and
//
//	reporting it as the finding is how a tool tells you to undo
//	work you have already done.
func SplitEnv(env map[string]string) (keys []string, safe map[string]string, secretLooking, byReference []string) {
	safe = map[string]string{}
	for k, v := range env {
		keys = append(keys, k)
		if IsSecretish(k) {
			if IsSecretReference(v) {
				byReference = append(byReference, k)
			} else if strings.TrimSpace(v) != "" {
				secretLooking = append(secretLooking, k)
			}
			// An EMPTY secret-looking variable is neither. Reporting it as a
			// plaintext secret is a false positive people learn to ignore,
			// which costs more than the one real finding it hides.
			continue
		}
		safe[k] = v
	}
	sortStrings(keys)
	sortStrings(secretLooking)
	sortStrings(byReference)
	return keys, safe, secretLooking, byReference
}

// IsSecretReference reports whether a value is a POINTER to a secret rather
// than the secret.
//
// ⛔ WHY THIS EXISTS. The capture deliberately records variable NAMES and not
// values, so the review could only judge by name -- and after the secrets were
// moved to SecureString references, it still reported every function as
// "holds secret values in environment variables". The tool described the fix
// as the defect.
//
// ⚠️ This reads the value's SHAPE and nothing else. A prefix is not a secret;
// the bytes after it are never stored, printed or returned.
func IsSecretReference(value string) bool {
	v := strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(v, "ssm:") ||
		strings.HasPrefix(v, "secretsmanager:") ||
		strings.HasPrefix(v, "arn:aws:secretsmanager:") ||
		strings.HasPrefix(v, "arn:aws:ssm:")
}
