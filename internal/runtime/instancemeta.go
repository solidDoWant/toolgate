package runtime

import (
	"maps"
	"strconv"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Labels and annotations toolgate puts on the namespace and on every object of
// an instance, so a cluster admin can tell whose it is.
const (
	LabelScope             = "toolgate.soliddowant.dev/scope"
	LabelTemplateNamespace = "toolgate.soliddowant.dev/template-namespace"
	LabelAttestation       = "toolgate.soliddowant.dev/attestation"

	AnnotationOwnerSubject       = "toolgate.soliddowant.dev/owner-subject"
	AnnotationOwnerEmail         = "toolgate.soliddowant.dev/owner-email"
	AnnotationOwnerName          = "toolgate.soliddowant.dev/owner-name"
	AnnotationWorkspace          = "toolgate.soliddowant.dev/workspace"
	AnnotationWorkspaceVerified  = "toolgate.soliddowant.dev/workspace-verified"
	AnnotationSession            = "toolgate.soliddowant.dev/session"
	AnnotationTemplateGeneration = "toolgate.soliddowant.dev/template-generation"
	AnnotationTemplateUID        = "toolgate.soliddowant.dev/template-uid"
	AnnotationCreatedBy          = "toolgate.soliddowant.dev/created-by"
)

// ownerHashLength is the number of hex characters of the owner label: the
// first 16 of SHA-256 of the owner's subject.
const ownerHashLength = 16

// OwnerID returns the label-safe short form of an owner's subject.
func OwnerID(subject string) string {
	return hashClaimed(subject)[:ownerHashLength]
}

// InstanceMetadata is the identifying labels and annotations of one instance.
type InstanceMetadata struct {
	Labels      map[string]string
	Annotations map[string]string
}

// NewInstanceMetadata builds the labels and annotations for an instance from
// verified data only: the stored instance row, the template and the session's
// identity. Shared instances get no owner, workspace or session keys. Label
// values are hashed or taken from validated names, so they always fit label
// rules.
func NewInstanceMetadata(instance db.Instance, template *v1alpha1.MCPServerTemplate, identity Identity,
	gatewayVersion string, omitOwnerDetails bool,
) InstanceMetadata {
	meta := InstanceMetadata{
		Labels: map[string]string{
			LabelScope:             instance.Scope,
			LabelTemplateNamespace: template.Namespace,
		},
		Annotations: map[string]string{
			AnnotationTemplateGeneration: strconv.FormatInt(instance.Generation, 10),
			AnnotationTemplateUID:        instance.TemplateUid,
			AnnotationCreatedBy:          gatewayVersion,
		},
	}

	if instance.Scope == scopeShared || instance.OwnerPrincipal == "" {
		return meta
	}

	meta.Labels[LabelOwner] = OwnerID(instance.OwnerPrincipal)
	meta.Annotations[AnnotationOwnerSubject] = instance.OwnerPrincipal

	if !omitOwnerDetails {
		if identity.PrincipalEmail != "" {
			meta.Annotations[AnnotationOwnerEmail] = identity.PrincipalEmail
		}

		if identity.PrincipalName != "" {
			meta.Annotations[AnnotationOwnerName] = identity.PrincipalName
		}
	}

	if instance.Scope != scopePerSession {
		return meta
	}

	if identity.Attested {
		meta.Labels[LabelAttestation] = "attested"
	} else {
		meta.Labels[LabelAttestation] = "user"
	}

	if identity.WorkspaceID != "" {
		meta.Annotations[AnnotationWorkspace] = identity.WorkspaceID
		meta.Annotations[AnnotationWorkspaceVerified] = strconv.FormatBool(identity.Attested)
	}

	if identity.SessionID != "" {
		meta.Annotations[AnnotationSession] = identity.SessionID
	}

	return meta
}

// mergeKeys returns base with extra on top: extra's keys win.
func mergeKeys(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	maps.Copy(out, base)
	maps.Copy(out, extra)

	return out
}
