// Package aspire discovers a running Aspire app's resolved endpoints over its
// gRPC resource service (aspire.v1.DashboardService). shunt uses this to learn
// which host:port each resource exposes, then proxies the stable front door
// onto them.
package aspire

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/gordonbeeming/shunt/internal/aspire/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// apiKeyHeader is the gRPC metadata key Aspire's dashboard uses to authenticate
// to the resource service. Empty key (unsecured transport) skips it.
const apiKeyHeader = "x-resource-service-api-key"

// Endpoint is a resolved Aspire endpoint from the live app.
type Endpoint struct {
	Resource string // Aspire resource name (e.g. "cache", "apiservice")
	Name     string // endpoint name within that resource (e.g. "http", "tcp")
	Scheme   string // http | https | tcp | ...
	Host     string // usually 127.0.0.1 inside the guest
	Port     int
	Internal bool // dashboard "internal" flag
}

// Discover connects to a running Aspire app's resource service at addr
// (host:port, plaintext h2c) and returns the endpoints from the initial
// snapshot of WatchResources. apiKey is sent as metadata when non-empty.
func Discover(ctx context.Context, addr, apiKey string) ([]Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial resource service %s: %w", addr, err)
	}
	defer conn.Close()

	if apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, apiKeyHeader, apiKey)
	}

	stream, err := pb.NewDashboardServiceClient(conn).WatchResources(ctx, &pb.WatchResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("watch resources: %w", err)
	}

	// The first streamed message carries the full initial snapshot.
	msg, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive initial resource snapshot: %w", err)
	}
	initial := msg.GetInitialData()
	if initial == nil {
		return nil, fmt.Errorf("first resource message had no initial data")
	}

	var endpoints []Endpoint
	for _, r := range initial.GetResources() {
		for _, u := range r.GetUrls() {
			if u.GetIsInactive() {
				continue
			}
			parsed, err := url.Parse(u.GetFullUrl())
			if err != nil {
				continue
			}
			port, err := portFor(parsed)
			if err != nil {
				continue
			}
			endpoints = append(endpoints, Endpoint{
				Resource: r.GetName(),
				Name:     u.GetEndpointName(),
				Scheme:   parsed.Scheme,
				Host:     parsed.Hostname(),
				Port:     port,
				Internal: u.GetIsInternal(),
			})
		}
	}
	return endpoints, nil
}

// portFor extracts the port from a parsed URL, defaulting by scheme when absent.
func portFor(u *url.URL) (int, error) {
	if p := u.Port(); p != "" {
		return strconv.Atoi(p)
	}
	switch u.Scheme {
	case "https":
		return 443, nil
	case "http":
		return 80, nil
	default:
		return 0, fmt.Errorf("no port in %q", u.String())
	}
}

// Find returns the endpoint matching a resource + endpoint name, or false. An
// empty endpointName matches the resource's first endpoint.
//
// Aspire 13 appends a unique suffix to resource names (e.g. "cache" becomes
// "cache-vdqvwwje"), so the contract's logical name matches either exactly or as
// the "<name>-" prefix.
func Find(eps []Endpoint, resource, endpointName string) (Endpoint, bool) {
	for _, e := range eps {
		if !resourceMatches(e.Resource, resource) {
			continue
		}
		if endpointName == "" || e.Name == endpointName {
			return e, true
		}
	}
	return Endpoint{}, false
}

func resourceMatches(actual, logical string) bool {
	return actual == logical || strings.HasPrefix(actual, logical+"-")
}
