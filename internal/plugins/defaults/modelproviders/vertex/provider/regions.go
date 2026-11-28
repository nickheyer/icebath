package provider

import "github.com/nickheyer/icebath/internal/config"

// Regions Vertex AI serves models in, global first
func regions() []config.Option {
	options := make([]config.Option, 0, len(vertexRegions))
	for _, region := range vertexRegions {
		options = append(options, config.Option{Value: region[0], Title: region[0], About: region[1]})
	}
	return options
}

// Regions of Vertex AI, each with the place Google names it after
var vertexRegions = [][2]string{
	{"global", "Google picks the region"}, {"us-central1", "Iowa"}, {"us-east1", "South Carolina"}, {"us-east4", "Northern Virginia"},
	{"us-east5", "Columbus"}, {"us-south1", "Dallas"}, {"us-west1", "Oregon"}, {"us-west4", "Las Vegas"},
	{"northamerica-northeast1", "Montreal"}, {"southamerica-east1", "Sao Paulo"}, {"europe-west1", "Belgium"}, {"europe-west2", "London"},
	{"europe-west3", "Frankfurt"}, {"europe-west4", "Netherlands"}, {"europe-west6", "Zurich"}, {"europe-west8", "Milan"},
	{"europe-west9", "Paris"}, {"europe-north1", "Finland"}, {"europe-southwest1", "Madrid"}, {"europe-central2", "Warsaw"},
	{"asia-east1", "Taiwan"}, {"asia-east2", "Hong Kong"}, {"asia-northeast1", "Tokyo"}, {"asia-northeast3", "Seoul"},
	{"asia-south1", "Mumbai"}, {"asia-southeast1", "Singapore"}, {"australia-southeast1", "Sydney"}, {"me-central1", "Doha"},
	{"me-central2", "Dammam"}, {"me-west1", "Tel Aviv"},
}
