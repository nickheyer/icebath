package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/credentials/aws"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// How long a look for AWS credentials outside the environment's variables is believed
const detectFor = time.Minute

// A Bedrock API key of the account or of AWS_BEARER_TOKEN_BEDROCK, else the AWS credential chain, which signs each request by SigV4
type source struct {
	mu sync.Mutex
	// Chain of each AWS profile, kept so its keys renew in place
	chains map[string]*aws.Chain
	// What the last look outside the variables found for each profile, and when
	found map[string]detected
}

type detected struct {
	source string
	at     time.Time
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{chains: map[string]*aws.Chain{}, found: map[string]detected{}})
}

// AWS profile the provider signs with, AWS_PROFILE and then default when it names none
func profile(account credentials.Account) string {
	return strings.TrimSpace(account.Provider.Settings.GetString(bedrockconfig.Profile))
}

// Variable of the account's profile environment
func lookup(account credentials.Account) func(string) (string, bool) {
	return account.Fields.LookupEnv
}

// Chain of the provider's AWS profile
func (s *source) chain(account credentials.Account) *aws.Chain {
	key := profile(account)
	s.mu.Lock()
	defer s.mu.Unlock()
	chain, ok := s.chains[key]
	if !ok {
		chain = aws.NewChain(aws.Options{Profile: key, Lookup: lookup(account)})
		s.chains[key] = chain
	}
	return chain
}

// Region requests of the provider are signed for: its setting, else AWS_REGION and AWS_DEFAULT_REGION, else its AWS profile's, else
// us-east-1
func (s *source) Region(account credentials.Account) string {
	if region := strings.TrimSpace(account.Provider.Settings.GetString(bedrockconfig.Region)); region != "" {
		return region
	}
	for _, name := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if v, _ := account.Fields.LookupEnv(name); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if region := s.chain(account).Region(); region != "" {
		return region
	}
	return "us-east-1"
}

// Which AWS sign-in is in use, the instance's role looked for at most once a minute; empty when none is
func (s *source) detect(account credentials.Account) string {
	if name := aws.Detect(context.Background(), lookup(account), false); name != "" {
		return name
	}
	key := profile(account)
	s.mu.Lock()
	last, ok := s.found[key]
	s.mu.Unlock()
	if ok && time.Since(last.at) < detectFor {
		return last.source
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	name := aws.Detect(ctx, lookup(account), true)
	s.mu.Lock()
	s.found[key] = detected{source: name, at: time.Now()}
	s.mu.Unlock()
	return name
}

func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != bedrockconfig.Name {
		return credentials.Credential{}, false
	}
	if credential, ok := account.Lookup(providerconfig.APIKeyField, bedrockconfig.APIKeyEnv); ok {
		credential.Auth = provider.AuthAPIKey
		credential.BaseURL, credential.Region = s.address(account), s.Region(account)
		return credential, true
	}
	name := s.detect(account)
	if name == "" || account.Ignores(name) {
		return credentials.Credential{}, false
	}
	origin := credentials.External
	if strings.HasPrefix(name, "AWS_") {
		origin = credentials.Environment
	}
	chain, region := s.chain(account), s.Region(account)
	return credentials.Credential{Auth: provider.AuthAWSSigV4, Origin: origin, Source: name, Sign: aws.Signer(chain, "bedrock", region),
		BaseURL: s.address(account), Region: region}, true
}

// Runtime address of the provider's region, unless the provider is set to an address of its own, such as a VPC endpoint
func (s *source) address(account credentials.Account) string {
	if base := strings.TrimSpace(account.Provider.Spec.BaseURL); base != "" && base != bedrockconfig.BaseURL {
		return base
	}
	return aws.RuntimeURL(s.Region(account))
}

// What AWS signs the provider in as: the sign-in in use, the region and the identity STS names
func (s *source) Report(ctx context.Context, account credentials.Account) ([]credentials.Detail, bool) {
	if account.Spec() != bedrockconfig.Name {
		return nil, false
	}
	if _, ok := account.Lookup(providerconfig.APIKeyField, bedrockconfig.APIKeyEnv); ok {
		return nil, true
	}
	name := s.detect(account)
	if name == "" {
		return nil, true
	}
	region := s.Region(account)
	details := []credentials.Detail{{Name: "Sign-in", Value: name}, {Name: "Region", Value: region}}
	if p := profile(account); p != "" {
		details = append(details, credentials.Detail{Name: "Profile", Value: p})
	}
	identity := "Not known, because AWS STS did not answer"
	if keys, err := s.chain(account).Retrieve(ctx); err == nil {
		if arn, err := aws.Identity(ctx, keys, region); err == nil {
			identity = arn
		}
	} else if needed, ok := errors.AsType[*aws.LoginNeeded](err); ok {
		identity = needed.Error()
	}
	return append(details, credentials.Detail{Name: "Identity", Value: identity}), true
}

// Why the account has no credential: no Bedrock API key and no AWS sign-in, or one the provider ignores
func (s *source) Why(account credentials.Account) (string, bool) {
	if account.Spec() != bedrockconfig.Name {
		return "", false
	}
	if _, ok := account.Lookup(providerconfig.APIKeyField, bedrockconfig.APIKeyEnv); ok {
		return "", false
	}
	id := account.Provider.ID
	switch name := s.detect(account); {
	case name == "":
		return fmt.Sprintf("Bedrock needs an AWS sign-in. Run `aws sso login` or `aws configure`, pick its AWS profile, or add a Bedrock API key "+
			"to %s.", id), true
	case account.Ignores(name):
		return fmt.Sprintf("This provider ignores the AWS sign-in of %s. Run `icebath auth add %s` to use it again, or add a Bedrock API key "+
			"with `icebath auth add %s --type api-key`.", name, id, id), true
	}
	return "", false
}
