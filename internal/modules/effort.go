package modules

import (
	"github.com/frank-bee/claude-statusline/internal/config"
	"github.com/frank-bee/claude-statusline/internal/input"
)

// EffortModule renders the effective reasoning effort level.
type EffortModule struct{}

func (EffortModule) Name() string { return "effort" }

func (EffortModule) Render(data input.Data, cfg config.Config) (string, error) {
	if data.Effort.Level == "" {
		return "", nil
	}

	templateData := struct{ Level string }{Level: data.Effort.Level}

	result, err := renderTemplate("effort", cfg.Effort.Format, templateData)
	if err != nil {
		return "", err
	}

	return wrapStyle(result, cfg.Effort.Style), nil
}
