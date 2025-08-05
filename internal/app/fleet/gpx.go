package fleet

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/whereiskurt/meshtk/internal/embedded/gpx"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type GPX struct {
	XMLName xml.Name   `xml:"gpx"`
	Trk     []Track    `xml:"trk"`
	Rte     []GPXRoute `xml:"rte"`
	Wpt     []Waypoint `xml:"wpt"`
}

type Track struct {
	Name    string     `xml:"name"`
	TrkSegs []TrackSeg `xml:"trkseg"`
}

type TrackSeg struct {
	TrkPts []TrackPoint `xml:"trkpt"`
}

type TrackPoint struct {
	Lat  float64 `xml:"lat,attr"`
	Lon  float64 `xml:"lon,attr"`
	Ele  float64 `xml:"ele"`
	Time string  `xml:"time"`
}

type GPXRoute struct {
	Name   string       `xml:"name"`
	Points []RoutePoint `xml:"rtept"`
}

type RoutePoint struct {
	Lat  float64 `xml:"lat,attr"`
	Lon  float64 `xml:"lon,attr"`
	Ele  float64 `xml:"ele"`
	Time string  `xml:"time"`
}

type Waypoint struct {
	Lat  float64 `xml:"lat,attr"`
	Lon  float64 `xml:"lon,attr"`
	Ele  float64 `xml:"ele"`
	Name string  `xml:"name"`
	Cmt  string  `xml:"cmt"`
	Desc string  `xml:"desc"`
}

// embeddedGPXMap caches the map of embedded GPX files
var embeddedGPXMap map[string][]byte
var embeddedGPXMapOnce sync.Once

func (f *FleetCmd) GPXCoords(gpxFilePath string) []config.Coordinate {
	var byteValue []byte
	var err error

	// First try to open the file from the filesystem
	xmlFile, err := os.Open(gpxFilePath)
	if err != nil {
		// File doesn't exist, try to use embedded file with the same base name
		baseName := filepath.Base(gpxFilePath)

		// Initialize the embedded GPX map once
		embeddedGPXMapOnce.Do(func() {
			embeddedGPXMap, err = gpx.GetEmbeddedGPXMap()
			if err != nil {
				f.Config.Log.Errorf("Failed to load embedded GPX files: %v", err)
				embeddedGPXMap = make(map[string][]byte)
			}
		})

		// Look up the file in our embedded map
		if content, ok := embeddedGPXMap[baseName]; ok {
			byteValue = content
		} else {
			f.Config.Log.Errorf("Failed to find GPX file either on filesystem or embedded: %s", gpxFilePath)
			return []config.Coordinate{}
		}
	} else {
		defer xmlFile.Close()
		byteValue, err = io.ReadAll(xmlFile)
		if err != nil {
			f.Config.Log.Errorf("Failed to read GPX file: %s, error: %v", gpxFilePath, err)
			return []config.Coordinate{}
		}
	}

	// Parse XML
	var gpx GPX
	err = xml.Unmarshal(byteValue, &gpx)
	if err != nil {
		f.Config.Log.Errorf("Failed to parse GPX file: %s, error: %v", gpxFilePath, err)
		return []config.Coordinate{}
	}

	// Extract coordinates
	var coordinates []config.Coordinate
	for _, track := range gpx.Trk {
		for _, segment := range track.TrkSegs {
			for _, point := range segment.TrkPts {
				// Convert to int32 format used by the application (multiplied by 10^7)
				lat := int32(point.Lat * 10000000)
				lon := int32(point.Lon * 10000000)
				alt := int32(point.Ele)

				coordinates = append(coordinates, config.Coordinate{
					Latitude:  lat,
					Longitude: lon,
					Altitude:  alt,
					Precision: 32,
				})
			}
		}
	}

	// If no tracks found, try routes
	if len(coordinates) == 0 {
		for _, route := range gpx.Rte {
			for _, point := range route.Points {
				// Convert to int32 format used by the application (multiplied by 10^7)
				lat := int32(point.Lat * 10000000)
				lon := int32(point.Lon * 10000000)
				alt := int32(point.Ele)

				coordinates = append(coordinates, config.Coordinate{
					Latitude:  lat,
					Longitude: lon,
					Altitude:  alt,
					Precision: 32,
				})
			}
		}
	}

	// If no tracks or routes found, try waypoints
	if len(coordinates) == 0 {
		for _, waypoint := range gpx.Wpt {
			// Convert to int32 format used by the application (multiplied by 10^7)
			lat := int32(waypoint.Lat * 10000000)
			lon := int32(waypoint.Lon * 10000000)
			alt := int32(waypoint.Ele)

			coordinates = append(coordinates, config.Coordinate{
				Latitude:  lat,
				Longitude: lon,
				Altitude:  alt,
				Precision: 32,
			})
		}
	}

	return coordinates
}
