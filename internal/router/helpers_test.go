package router

import (
	"github.com/yourorg/conduit/internal/canonical"
	"github.com/yourorg/conduit/internal/config"
)

func requestFor(tools bool, inTok int) *canonical.Request {
	return &canonical.Request{
		Requested:   "auto",
		Tools:       tools,
		InTokensEst: inTok,
		MaxOutput:   64,
		Task:        "chat",
		TaskConf:    0.5,
		Metadata:    map[string]string{},
	}
}

func hasCap(m *config.Model, c string) bool {
	for _, k := range m.Capabilities {
		if k == c {
			return true
		}
	}
	return false
}
