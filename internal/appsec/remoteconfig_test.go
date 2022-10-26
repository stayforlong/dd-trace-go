// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

//go:build appsec
// +build appsec

package appsec

import (
	"os"
	"testing"

	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
	"github.com/stretchr/testify/require"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/waf"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/remoteconfig"
)

func TestASMFeaturesCallback(t *testing.T) {
	if waf.Health() != nil {
		t.Skip("WAF cannot be used")
	}
	enabledPayload := []byte(`{"asm":{"enabled":true}}`)
	disabledPayload := []byte(`{"asm":{"enabled":false}}`)
	cfg, err := newConfig()
	require.NoError(t, err)
	a := newAppSec(cfg)

	t.Setenv(enabledEnvVar, "")
	os.Unsetenv(enabledEnvVar)

	for _, tc := range []struct {
		name   string
		update remoteconfig.ProductUpdate
		// Should appsec be started before beginning the test
		startBefore bool
		// Is appsec expected to be started at the end of the test
		startedAfter bool
	}{
		{
			// This case shouldn't happen due to how callbacks dispatch work, but better safe than sorry
			name: "empty-update",
		},
		{
			name:         "enabled",
			update:       remoteconfig.ProductUpdate{"some/path": enabledPayload},
			startedAfter: true,
		},
		{
			name:        "disabled",
			update:      remoteconfig.ProductUpdate{"some/path": disabledPayload},
			startBefore: true,
		},
		{
			name:   "several-configs-1",
			update: remoteconfig.ProductUpdate{"some/path/1": disabledPayload, "some/path/2": enabledPayload},
		},
		{
			name:         "several-configs-2",
			update:       remoteconfig.ProductUpdate{"some/path/1": disabledPayload, "some/path/2": enabledPayload},
			startBefore:  true,
			startedAfter: true,
		},
		{
			name:   "bad-config-1",
			update: remoteconfig.ProductUpdate{"some/path": []byte("ImABadPayload")},
		},
		{
			name:         "bad-config-2",
			update:       remoteconfig.ProductUpdate{"some/path": []byte("ImABadPayload")},
			startBefore:  true,
			startedAfter: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer a.stop()
			require.NotNil(t, a)
			if tc.startBefore {
				a.start()
			}
			require.Equal(t, tc.startBefore, a.started)
			a.asmFeaturesCallback(tc.update)
			require.Equal(t, tc.startedAfter, a.started)
		})
	}

	t.Run("enabled-twice", func(t *testing.T) {
		defer a.stop()
		update := remoteconfig.ProductUpdate{"some/path": enabledPayload}
		require.False(t, a.started)
		a.asmFeaturesCallback(update)
		require.True(t, a.started)
		a.asmFeaturesCallback(update)
		require.True(t, a.started)
	})
	t.Run("disabled-twice", func(t *testing.T) {
		defer a.stop()
		update := remoteconfig.ProductUpdate{"some/path": disabledPayload}
		require.False(t, a.started)
		a.asmFeaturesCallback(update)
		require.False(t, a.started)
		a.asmFeaturesCallback(update)
		require.False(t, a.started)
	})
}

// This test ensures that the remote activation capabilities are only set if DD_APPSEC_ENABLED is not set in the env.
func TestRemoteActivationScenarios(t *testing.T) {
	if waf.Health() != nil {
		t.Skip("WAF cannot be used")
	}

	t.Run("DD_APPSEC_ENABLED unset", func(t *testing.T) {
		t.Setenv(enabledEnvVar, "")
		os.Unsetenv(enabledEnvVar)
		Start(WithRCConfig(remoteconfig.DefaultClientConfig()))
		defer Stop()

		require.NotNil(t, activeAppSec)
		require.False(t, Enabled())
		client := activeAppSec.rc
		require.NotNil(t, client)
		require.Contains(t, client.Capabilities, remoteconfig.ASMActivation)
		require.Contains(t, client.Products, rc.ProductASMFeatures)
	})

	t.Run("DD_APPSEC_ENABLED=true", func(t *testing.T) {
		t.Setenv(enabledEnvVar, "true")
		Start(WithRCConfig(remoteconfig.DefaultClientConfig()))
		defer Stop()

		require.True(t, Enabled())
		client := activeAppSec.rc
		require.NotNil(t, client)
		require.NotContains(t, client.Capabilities, remoteconfig.ASMActivation)
		require.NotContains(t, client.Products, rc.ProductASMFeatures)
	})

	t.Run("DD_APPSEC_ENABLED=false", func(t *testing.T) {
		t.Setenv(enabledEnvVar, "false")
		Start(WithRCConfig(remoteconfig.DefaultClientConfig()))
		defer Stop()
		require.Nil(t, activeAppSec)
		require.False(t, Enabled())
	})
}