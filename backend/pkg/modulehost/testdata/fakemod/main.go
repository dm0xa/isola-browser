// Command fakemod is a test double: a minimal contract v1 module that
// allows every session creation and blocks one URL suffix. Built and
// launched by the modulehost integration test.
package main

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/modulesdk"
	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
)

type allowGuard struct{}

func (allowGuard) Info() plugin.Info { return plugin.Info{Name: "fakemod"} }
func (allowGuard) BeforeCreate(context.Context, *http.Request, uuid.UUID, string) error {
	return nil
}

func main() {
	if err := modulesdk.Run(modulesdk.Info{
		Name:        "fakemod",
		Version:     "0.0.1",
		Description: "integration test module",
	}, func(s *modulesdk.Server) {
		s.SetSessionGuard(allowGuard{})
	}); err != nil {
		panic(err)
	}
}
