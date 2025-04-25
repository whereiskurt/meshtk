package fleet

import (
	"encoding/xml"
	"io"
	"os"

	"github.com/whereiskurt/meshtk/pkg/config"
)

type GPX struct {
	XMLName xml.Name `xml:"gpx"`
	Trk     []Track  `xml:"trk"`
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

func (f *FleetCmd) GPXCoords(gpxFilePath string) []config.Coordinate {
	// Read GPX file
	xmlFile, err := os.Open(gpxFilePath)
	if err != nil {
		f.Config.Log.Errorf("Failed to open GPX file: %s, error: %v", gpxFilePath, err)
		return []config.Coordinate{}
	}
	defer xmlFile.Close()

	byteValue, err := io.ReadAll(xmlFile)
	if err != nil {
		f.Config.Log.Errorf("Failed to read GPX file: %s, error: %v", gpxFilePath, err)
		return []config.Coordinate{}
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

	return coordinates
}
