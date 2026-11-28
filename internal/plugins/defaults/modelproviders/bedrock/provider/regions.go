package provider

import (
	"os"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials/aws"
)

// Regions a Bedrock provider can send its requests to, after the choice that leaves it to the AWS settings
func regions() []config.Option {
	options := []config.Option{{Title: "From AWS settings", About: "AWS_REGION, AWS_DEFAULT_REGION or the AWS profile decides"}}
	for _, region := range bedrockRegions {
		options = append(options, config.Option{Value: region[0], Title: region[0], About: region[1]})
	}
	return options
}

// Regions Bedrock runs in, each with the place AWS names it after
var bedrockRegions = [][2]string{
	{"us-east-1", "US East, N. Virginia"}, {"us-east-2", "US East, Ohio"}, {"us-west-1", "US West, N. California"},
	{"us-west-2", "US West, Oregon"}, {"us-gov-east-1", "AWS GovCloud, US East"}, {"us-gov-west-1", "AWS GovCloud, US West"},
	{"ca-central-1", "Canada, Central"}, {"ca-west-1", "Canada, Calgary"}, {"mx-central-1", "Mexico, Central"},
	{"sa-east-1", "South America, Sao Paulo"}, {"eu-central-1", "Europe, Frankfurt"}, {"eu-central-2", "Europe, Zurich"},
	{"eu-west-1", "Europe, Ireland"}, {"eu-west-2", "Europe, London"}, {"eu-west-3", "Europe, Paris"}, {"eu-north-1", "Europe, Stockholm"},
	{"eu-south-1", "Europe, Milan"}, {"eu-south-2", "Europe, Spain"}, {"il-central-1", "Israel, Tel Aviv"}, {"me-central-1", "Middle East, UAE"},
	{"me-south-1", "Middle East, Bahrain"}, {"af-south-1", "Africa, Cape Town"}, {"ap-south-1", "Asia Pacific, Mumbai"},
	{"ap-south-2", "Asia Pacific, Hyderabad"}, {"ap-east-2", "Asia Pacific, Taipei"}, {"ap-northeast-1", "Asia Pacific, Tokyo"},
	{"ap-northeast-2", "Asia Pacific, Seoul"}, {"ap-northeast-3", "Asia Pacific, Osaka"}, {"ap-southeast-1", "Asia Pacific, Singapore"},
	{"ap-southeast-2", "Asia Pacific, Sydney"}, {"ap-southeast-3", "Asia Pacific, Jakarta"}, {"ap-southeast-4", "Asia Pacific, Melbourne"},
	{"ap-southeast-5", "Asia Pacific, Malaysia"}, {"ap-southeast-7", "Asia Pacific, Thailand"},
}

// Profiles of the AWS settings on this computer, read each time they are shown, after the choice that leaves it to AWS_PROFILE
func profiles() []config.Option {
	options := []config.Option{{Title: "From AWS settings", About: "AWS_PROFILE decides, else the profile named default"}}
	for _, name := range aws.SharedProfiles(os.Getenv) {
		options = append(options, config.Option{Value: name, Title: name})
	}
	return options
}
