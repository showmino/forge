package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

const templatePrefix = "templates/"

// templateService serves TemplateService from a TemplateSource. Any
// authenticated caller may read templates.
type templateService struct {
	src TemplateSource
	log *slog.Logger
}

func authenticated(ctx context.Context) error {
	if _, ok := PrincipalFrom(ctx); !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	return nil
}

// release resolves version (empty = newest) and fetches its tree, for any
// authenticated caller.
func (s *templateService) release(ctx context.Context, version string) (string, fs.FS, error) {
	if err := authenticated(ctx); err != nil {
		return "", nil, err
	}
	if s.src == nil {
		return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	return resolveRelease(ctx, s.src, version)
}

// resolveRelease resolves version (empty = newest) and fetches its tree.
// Errors are Connect errors.
func resolveRelease(ctx context.Context, src TemplateSource, version string) (string, fs.FS, error) {
	if version == "" {
		vs, err := src.Versions(ctx)
		if err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list template versions: %w", err))
		}
		if len(vs) == 0 {
			return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the template repo has no release tags yet"))
		}
		version = vs[0]
	}
	tree, err := src.Fetch(ctx, version)
	switch {
	case errors.Is(err, ErrVersionNotFound):
		return "", nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("template version %s not found", version))
	case errors.Is(err, ErrInvalidRelease):
		return "", nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", nil, connect.NewError(connect.CodeDeadlineExceeded, err)
	case err != nil:
		return "", nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("template source unavailable: %w", err))
	}
	return version, tree, nil
}

var errTemplateNotFound = errors.New("template not found")

// templateFromRelease loads template name from a release tree, as a Connect error.
func templateFromRelease(tree fs.FS, name, version string) (*template.Template, error) {
	t, err := loadTemplate(tree, name)
	if errors.Is(err, errTemplateNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found in %s", templatePrefix, name, version))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s in %s is invalid: %w", templatePrefix, name, version, err))
	}
	return t, nil
}

// loadTemplate parses and validates template name in tree.
func loadTemplate(tree fs.FS, name string) (*template.Template, error) {
	data, err := fs.ReadFile(tree, path.Join(name, template.FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := template.Parse(data)
	if err != nil {
		return nil, err
	}
	if findings := t.Validate(); len(findings) > 0 {
		return nil, errors.New(findings[0].String())
	}
	if t.Name != name {
		return nil, fmt.Errorf("template.yaml names %q, not %q", t.Name, name)
	}
	return t, nil
}

func (s *templateService) ListTemplates(ctx context.Context, req *connect.Request[forgev1.ListTemplatesRequest]) (*connect.Response[forgev1.ListTemplatesResponse], error) {
	version, tree, err := s.release(ctx, req.Msg.GetVersion())
	if err != nil {
		return nil, err
	}
	names, err := template.Discover(tree)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read templates of %s: %w", version, err))
	}
	resp := &forgev1.ListTemplatesResponse{}
	for _, name := range names {
		t, err := loadTemplate(tree, name)
		if err != nil {
			s.log.WarnContext(ctx, "skipping invalid template", "template", name, "version", version, "error", err)
			continue
		}
		if tag := req.Msg.GetTag(); tag != "" && !slices.Contains(t.Tags, tag) {
			continue
		}
		resp.Templates = append(resp.Templates, templateProto(t, version))
	}
	return connect.NewResponse(resp), nil
}

func (s *templateService) GetTemplate(ctx context.Context, req *connect.Request[forgev1.GetTemplateRequest]) (*connect.Response[forgev1.GetTemplateResponse], error) {
	name := strings.TrimPrefix(req.Msg.GetName(), templatePrefix)
	if !manifest.ValidID(name) {
		return nil, invalid(fmt.Sprintf("name %q must look like templates/{template}", req.Msg.GetName()))
	}
	version, tree, err := s.release(ctx, req.Msg.GetVersion())
	if err != nil {
		return nil, err
	}
	t, err := templateFromRelease(tree, name, version)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetTemplateResponse{Template: templateProto(t, version)}), nil
}

var paramTypes = map[template.ParamType]forgev1.TemplateParameter_Type{
	template.TypeString: forgev1.TemplateParameter_TYPE_STRING,
	template.TypeInt:    forgev1.TemplateParameter_TYPE_INT,
	template.TypeBool:   forgev1.TemplateParameter_TYPE_BOOL,
	template.TypeEnum:   forgev1.TemplateParameter_TYPE_ENUM,
	template.TypeList:   forgev1.TemplateParameter_TYPE_LIST,
}

func templateProto(t *template.Template, version string) *forgev1.Template {
	out := &forgev1.Template{
		Name:        templatePrefix + t.Name,
		Version:     version,
		Title:       t.Title,
		Description: t.Description,
		Tags:        slices.Clone(t.Tags),
		Owner:       t.Owner,
		Options:     slices.Clone(t.Options),
		Type:        template.TypeService,
		TargetRepo:  t.Target,
	}
	if t.IsFile() {
		out.Type = template.TypeFile
	}
	for _, p := range t.Parameters {
		def, _ := p.DefaultString()
		tp := &forgev1.TemplateParameter{
			Name:         p.Name,
			Type:         paramTypes[p.Type],
			Required:     p.Required(),
			DefaultValue: def,
			Pattern:      p.Pattern,
			Values:       slices.Clone(p.Values),
			Description:  p.Description,
			Label:        p.Label,
			Placeholder:  p.Placeholder,
			Group:        p.Group,
		}
		if p.Min != nil {
			tp.Min = proto.Int64(int64(*p.Min))
		}
		if p.Max != nil {
			tp.Max = proto.Int64(int64(*p.Max))
		}
		out.Parameters = append(out.Parameters, tp)
	}
	return out
}
