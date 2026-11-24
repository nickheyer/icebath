package timeutil

// Embedded zone database time.LoadLocation falls back to on hosts without one, Windows and minimal containers among them
import _ "time/tzdata"
