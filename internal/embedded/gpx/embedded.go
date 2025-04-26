package gpx

import (
	"embed"
	"path/filepath"
)

//go:embed *.gpx
var EmbeddedGPXFiles embed.FS

// GetEmbeddedGPXContent returns the content of an embedded GPX file by name
func GetEmbeddedGPXContent(name string) ([]byte, error) {
	return EmbeddedGPXFiles.ReadFile(name)
}

// GetEmbeddedGPXMap returns a map of filename to file content
func GetEmbeddedGPXMap() (map[string][]byte, error) {
	entries, err := EmbeddedGPXFiles.ReadDir(".")
	if err != nil {
		return nil, err
	}

	gpxMap := make(map[string][]byte)
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".gpx" {
			content, err := EmbeddedGPXFiles.ReadFile(entry.Name())
			if err != nil {
				continue
			}
			gpxMap[entry.Name()] = content
		}
	}

	return gpxMap, nil
}

// ListEmbeddedGPXFiles returns a list of all embedded GPX files
func ListEmbeddedGPXFiles() ([]string, error) {
	entries, err := EmbeddedGPXFiles.ReadDir(".")
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".gpx" {
			files = append(files, entry.Name())
		}
	}

	return files, nil
}
