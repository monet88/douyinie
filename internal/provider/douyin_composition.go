package provider

import (
	"os"
	"strings"
)

// RegisterProductionDouyinProviders keeps desktop and standalone RuntimeHost on
// one provider composition. Missing optional helpers simply leave that lane
// unavailable; manual/local-file flows remain usable.
func RegisterProductionDouyinProviders(reg *Registry, resolver SecretResolver) error {
	if reg == nil {
		return nil
	}
	python := strings.TrimSpace(os.Getenv("DOUYINIE_JIJI_PYTHON"))
	if python == "" {
		python = "python"
	}
	jijiScript := strings.TrimSpace(os.Getenv("DOUYINIE_JIJI_SCRIPT"))
	if jijiScript != "" {
		if err := reg.Register(NewJijiAdapter("v2", python, jijiScript, resolver)); err != nil {
			return err
		}
		if err := reg.Register(NewJijiDiscoveryAdapter("v2", python, jijiScript, resolver)); err != nil {
			return err
		}
	}
	if helper := strings.TrimSpace(os.Getenv("DOUYINIE_DOUYIN_PAGE_HELPER")); helper != "" {
		if err := reg.Register(NewPageBackedJijiAdapter("v1", helper, resolver)); err != nil {
			return err
		}
	}
	verified := strings.TrimSpace(os.Getenv("DOUYINIE_F2_ARGUS_VERIFIED"))
	if f2 := strings.TrimSpace(os.Getenv("DOUYINIE_F2_BIN")); f2 != "" && verified == "1" {
		if err := reg.Register(NewF2Adapter("v0", f2, resolver)); err != nil {
			return err
		}
	}
	return nil
}
