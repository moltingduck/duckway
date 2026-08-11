package client

import "github.com/hackerduck/duckway/internal/projectregistry"

type CCProject = projectregistry.Project
type CCProjectStore = projectregistry.Store

func NewCCProjectStore(configDir string) *CCProjectStore {
	return projectregistry.NewStore(configDir)
}

var ResolveProjectPaths = projectregistry.ResolveProjectPaths
var normalizeProjectPattern = projectregistry.NormalizeProjectPattern
