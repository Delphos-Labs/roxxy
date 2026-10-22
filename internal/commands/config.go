package commands

import (
	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
)

// Config holds common dependencies required by command handlers
type Config struct {
	CacheDir     string
	CertsDir     string
	LogDir       string
	ConfigFile   string
	StatsTracker *cache.StatsTracker
	IndexDB      *index.IndexDB
	BuildTime    string
	GlobalConfig *config.GlobalConfig
}
