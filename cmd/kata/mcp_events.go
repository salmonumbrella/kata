package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os/user"
	"path/filepath"
	"strings"

	internalclient "go.kenn.io/kata/internal/client"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/mcpevents"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func mcpEventsBinding(resolved internalclient.ResolvedDaemon, instance generated.InstanceResponse, actor, inboundToken string, mode mcpserver.ScopeMode, projectUIDs []string) (mcpevents.Binding, error) {
	if instance.InstanceUID == "" || instance.Auth.Kind == "" {
		return mcpevents.Binding{}, errors.New("Events requires an authenticated daemon identity")
	}
	var origin string
	if resolved.Network == "unix" {
		if !filepath.IsAbs(resolved.UnixSocket) {
			return mcpevents.Binding{}, errors.New("Events requires an absolute backend socket")
		}
		origin = "unix://" + filepath.Clean(resolved.UnixSocket)
	} else {
		normalized, err := internalclient.NormalizeRemoteURL(resolved.BaseURL, true)
		if err != nil {
			return mcpevents.Binding{}, errors.New("Events requires a canonical backend origin")
		}
		parsed, err := url.Parse(normalized)
		if err != nil || parsed.User != nil {
			return mcpevents.Binding{}, errors.New("Events requires a canonical backend origin")
		}
		host := strings.ToLower(parsed.Hostname())
		port := parsed.Port()
		if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
			port = ""
		}
		if port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		origin = parsed.Scheme + "://" + host
	}
	digest := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	transport := "http:" + digest(inboundToken)
	if inboundToken == "" {
		current, err := user.Current()
		if err != nil {
			return mcpevents.Binding{}, errors.New("Events requires a stable local process identity")
		}
		transport = "stdio:" + current.Uid
	}
	credential := "bearer:" + resolved.Token
	if resolved.Token == "" {
		credential = "tokenless:" + instance.Auth.Kind
	}
	return mcpevents.Binding{BackendOrigin: origin, DaemonUID: instance.InstanceUID, BackendCredentialFingerprint: digest(credential), BackendPrincipal: mcpserver.EventsPrincipalFingerprint(instance.Auth), TransportPrincipal: transport, Actor: actor, ScopeMode: string(mode), ProjectUIDs: append([]string(nil), projectUIDs...)}, nil
}

func configureMCPEvents(ctx context.Context, dir string, resolved internalclient.ResolvedDaemon, client *kataclient.Client, scope *mcpserver.Scope, actor, inboundToken string) (*mcpevents.Service, error) {
	instance, err := client.Instance(ctx)
	if err != nil {
		return nil, errors.New("Events could not authorize the selected daemon identity")
	}
	var projectUIDs []string
	if scope.Mode() != mcpserver.ScopeAll {
		// Bind the configured UID allowlist. Temporary catalog visibility changes
		// are reconciled by the source and cannot redefine this service identity.
		projects, err := scope.Projects(ctx, nil, false)
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			projectUIDs = append(projectUIDs, project.UID)
		}
	}
	binding, err := mcpEventsBinding(resolved, *instance, actor, inboundToken, scope.Mode(), projectUIDs)
	if err != nil {
		return nil, err
	}
	state, err := mcpevents.OpenState(dir, binding)
	if err != nil {
		return nil, err
	}
	service, err := mcpevents.NewService(mcpevents.ServiceOptions{State: state, Source: mcpserver.NewEventsSource(client, scope, instance.InstanceUID, instance.Auth), Webhook: mcpevents.NewWebhookClient()})
	if err != nil {
		_ = state.Close()
		return nil, err
	}
	return service, nil
}
