package ses

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sivchari/kumo/internal/service"
)

// Compile-time check that Service implements io.Closer.
var _ io.Closer = (*Service)(nil)

func init() {
	var opts []Option
	if dir := os.Getenv("KUMO_DATA_DIR"); dir != "" {
		opts = append(opts, WithDataDir(dir))
	}

	service.Register(New(NewMemoryStorage(opts...)))
}

// Service implements the SES v1 service.
type Service struct {
	storage Storage
}

// New creates a new SES v1 service.
func New(storage Storage) *Service {
	return &Service{
		storage: storage,
	}
}

// Name returns the service name.
func (s *Service) Name() string {
	return "ses"
}

// RegisterRoutes registers the SES v1 routes.
// SES v1 uses Query protocol, so most routes are handled via DispatchAction.
// The mailbox endpoint is registered here as a kumo-specific REST endpoint.
func (s *Service) RegisterRoutes(r service.Router) {
	// kumo-specific endpoint for local mailbox testing.
	r.HandleFunc("GET", "/_aws/ses", s.GetMailbox)
}

// TargetPrefix returns the target prefix for SES v1.
func (s *Service) TargetPrefix() string {
	return "SimpleEmailService"
}

// Actions returns the list of action names this service handles.
func (s *Service) Actions() []string {
	return []string{
		"VerifyEmailIdentity",
		"SendEmail",
		"SendRawEmail",
		"ListIdentities",
		"DeleteIdentity",
		"GetIdentityVerificationAttributes",
	}
}

// ServiceIdentifier returns the SDK service identifier for User-Agent disambiguation.
func (s *Service) ServiceIdentifier() string {
	return "ses"
}

// QueryProtocol is a marker method that indicates SES v1 uses AWS Query protocol.
func (s *Service) QueryProtocol() {}

// Close saves the storage state if persistence is enabled.
func (s *Service) Close() error {
	if c, ok := s.storage.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("failed to close storage: %w", err)
		}
	}

	return nil
}

// Meta returns the service's documentation metadata.
func (s *Service) Meta() service.Meta {
	return service.Meta{
		Display:     "SES",
		Category:    "Application Integration",
		Description: "Email service",
	}
}

// StorageBackend exposes the service's storage for in-process cross-service
// integration (route66 fork: CloudFormation resource materialization / SES
// store unification). Not part of upstream kumo.
func (s *Service) StorageBackend() Storage {
	return s.storage
}

// RecordEmail stores an already-sent email in the message store, preserving
// the caller-assigned MessageID. Used by the SESv2 service (route66 fork) to
// mirror v2 sends into the single v1 mailbox store.
func (s *Service) RecordEmail(ctx context.Context, email *SentEmail) {
	id := email.MessageID
	// storage.SendEmail assigns a fresh MessageID to the stored record; the
	// record is the same pointer, so restore the v2-assigned id afterwards
	// to keep the two APIs' MessageIds consistent.
	_, _ = s.storage.SendEmail(ctx, email)

	if id != "" {
		email.MessageID = id
	}
}
