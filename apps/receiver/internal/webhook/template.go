package webhook

import (
	_ "embed"
)

// jobTemplate is the Job manifest rendered for every pull_request webhook.
// The receiver substitutes __OWNER__, __REPO__, __NUMBER__, __SHA__, __SHA7__,
// __BASE_REF__, __PREVIOUS_HEAD_SHA__, __IMAGE__, __BOT_LOGIN__ at submit
// time. The double-underscore form survives prettier formatting without
// being rewritten as JSX-style braces.
//
//go:embed job-template.yaml
var jobTemplate string
