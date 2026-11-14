package web

import (
	"os"

	"ratingengine/internal/store"
	"ratingengine/internal/tariff"
)

func tariffLoad(st *store.Store) (*tariff.Catalog, error) {
	return tariff.Load(st)
}

func readDirNames(dir string) ([]os.DirEntry, error) {
	return os.ReadDir(dir)
}
