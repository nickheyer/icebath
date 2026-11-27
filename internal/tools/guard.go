package tools

import "github.com/nickheyer/icebath/internal/config"

// Registrant that guards paths of this machine, which the tools of other plugins that write or read a file of the model's choosing ask too,
// such as text_to_speech writing its audio: the file tools' credential stores, system files and write safe root
type Guard interface {
	// Why a write to the path is refused or must be approved, empty when it may go ahead
	Unwritable(cfg *config.Config, path string) string
	// Why a read of the path is refused, empty when it may go ahead
	Unreadable(cfg *config.Config, path string) string
}

// Refusal of a write to the path by the first registrant that refuses it, empty when none does
func (r *Registry) Unwritable(cfg *config.Config, path string) string {
	for _, registrant := range r.registrants {
		if guard, ok := registrant.(Guard); ok {
			if refusal := guard.Unwritable(cfg, path); refusal != "" {
				return refusal
			}
		}
	}
	return ""
}

// Refusal of a read of the path by the first registrant that refuses it, empty when none does
func (r *Registry) Unreadable(cfg *config.Config, path string) string {
	for _, registrant := range r.registrants {
		if guard, ok := registrant.(Guard); ok {
			if refusal := guard.Unreadable(cfg, path); refusal != "" {
				return refusal
			}
		}
	}
	return ""
}
