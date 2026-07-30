package webhook

import (
	_ "embed"
)

// jobTemplate is the Job manifest rendered for every pull_request webhook.
// Edited at apps/dev-tools/boop/base/job-template.yaml; the receiver
// substitutes __OWNER__, __REPO__, __NUMBER__, __SHA__, __SHA7__, __BASE_REF__,
// __IMAGE__ at submit time. The double-underscore form survives prettier
// formatting without being rewritten as JSX-style braces.
//
//go:embed job-template.yaml
var jobTemplate string
