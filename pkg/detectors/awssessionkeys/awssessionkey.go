package awssessionkey

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/detectors"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/detectorspb"
)

type scanner struct {
	verificationClient *http.Client
	skipIDs            map[string]struct{}
}

func New(opts ...func(*scanner)) *scanner {
	scanner := &scanner{
		skipIDs: map[string]struct{}{},
	}
	for _, opt := range opts {

		opt(scanner)
	}

	return scanner
}

func WithSkipIDs(skipIDs []string) func(*scanner) {
	return func(s *scanner) {
		ids := map[string]struct{}{}
		for _, id := range skipIDs {
			ids[id] = struct{}{}
		}

		s.skipIDs = ids
	}
}

// Ensure the scanner satisfies the interface at compile time.
var _ detectors.Detector = (*scanner)(nil)

var (
	defaultVerificationClient = common.SaneHttpClient()

	// Make sure that your group is surrounded in boundary characters such as below to reduce false positives.
	// Key types are from this list https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html#identifiers-unique-ids
	idPat      = regexp.MustCompile(`\b((?:ASIA)[0-9A-Z]{16})\b`)
	secretPat  = regexp.MustCompile(`\b[^A-Za-z0-9+\/]{0,1}([A-Za-z0-9+\/]{40})[^A-Za-z0-9+\/]{0,1}\b`)
	sessionPat = regexp.MustCompile(`\b[^A-Za-z0-9+\/]{0,1}([A-Za-z0-9+=\/]{41,1000})[^A-Za-z0-9+=\/]{0,1}\b`)
	// Hashes, like those for git, do technically match the secret pattern.
	// But they are extremely unlikely to be generated as an actual AWS secret.
	// So when we find them, if they're not verified, we should ignore the result.
	falsePositiveSecretCheck = regexp.MustCompile(`[a-f0-9]{40}`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s scanner) Keywords() []string {
	return []string{
		"ASIA",
	}
}

func GetHash(input string) string {
	data := []byte(input)
	hasher := sha256.New()
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

func GetHMAC(key []byte, data []byte) []byte {
	hasher := hmac.New(sha256.New, key)
	hasher.Write(data)
	return hasher.Sum(nil)
}

func checkSessionToken(sessionToken string, secret string) bool {
	if !strings.Contains(sessionToken, "YXdz") || strings.Contains(sessionToken, secret) {
		// Handle error if the sessionToken is not a valid base64 string
		return false
	}
	return true
}

// FromData will find and optionally verify AWS secrets in a given set of bytes.
func (s scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	idMatches := idPat.FindAllStringSubmatch(dataStr, -1)
	secretMatches := secretPat.FindAllStringSubmatch(dataStr, -1)
	sessionMatches := sessionPat.FindAllStringSubmatch(dataStr, -1)

	for _, idMatch := range idMatches {
		if len(idMatch) != 2 {
			continue
		}
		resIDMatch := strings.TrimSpace(idMatch[1])

		if s.skipIDs != nil {
			if _, ok := s.skipIDs[resIDMatch]; ok {
				continue
			}
		}

		for _, secretMatch := range secretMatches {
			if len(secretMatch) != 2 {
				continue
			}
			resSecretMatch := strings.TrimSpace(secretMatch[1])

			for _, sessionMatch := range sessionMatches {
				if len(sessionMatch) != 2 {
					continue
				}
				resSessionMatch := strings.TrimSpace(sessionMatch[1])
				if !checkSessionToken(resSessionMatch, resSecretMatch) {
					continue
				}
				s1 := detectors.Result{
					DetectorType: detectorspb.DetectorType_AWSSessionKey,
					Raw:          []byte(resIDMatch),
					Redacted:     resIDMatch,
					RawV2:        []byte(resIDMatch + resSecretMatch + resSessionMatch),
					ExtraData:    make(map[string]string),
				}

				if verify {
					isVerified, extraData, verificationErr := s.verifyMatch(ctx, resIDMatch, resSecretMatch, resSessionMatch, true)
					s1.Verified = isVerified
					if extraData != nil {
						s1.ExtraData = extraData
					}
					s1.SetVerificationError(verificationErr, resSecretMatch)
				}

				if !s1.Verified {
					// Unverified results that contain common test words are probably not secrets
					if detectors.IsKnownFalsePositive(resSecretMatch, detectors.DefaultFalsePositives, true) {
						continue
					}
					// Unverified results that look like hashes are probably not secrets
					if falsePositiveSecretCheck.MatchString(resSecretMatch) {
						continue
					}
				}

				// If we haven't already found an account number for this ID (via API), calculate one.
				if _, ok := s1.ExtraData["account"]; !ok {
					account, err := common.GetAccountNumFromAWSID(resIDMatch)
					if err == nil {
						s1.ExtraData["account"] = account
					}
				}

				results = append(results, s1)
				// If we've found a verified match with this ID, we don't need to look for any more. So move on to the next ID.
				if s1.Verified {
					break
				}
			}
		}
	}
	return awsCustomCleanResults(results), nil
}

func (s scanner) verifyMatch(ctx context.Context, resIDMatch, resSecretMatch string, resSessionMatch string, retryOn403 bool) (bool, map[string]string, error) {

	// REQUEST VALUES.
	method := "GET"
	service := "sts"
	host := "sts.amazonaws.com"
	region := "us-east-1"
	endpoint := "https://sts.amazonaws.com"
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Accept", "application/json")

	canonicalURI := "/"
	canonicalHeaders := "host:" + host + "\n" + "x-amz-date:" + amzDate + "\n" + "x-amz-security-token:" + resSessionMatch + "\n"
	signedHeaders := "host;x-amz-date;x-amz-security-token"
	algorithm := "AWS4-HMAC-SHA256"
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)

	params := req.URL.Query()
	params.Add("Action", "GetCallerIdentity")
	params.Add("Version", "2011-06-15")
	canonicalQuerystring := params.Encode()
	payloadHash := GetHash("") // empty payload
	canonicalRequest := method + "\n" + canonicalURI + "\n" + canonicalQuerystring + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash

	stringToSign := algorithm + "\n" + amzDate + "\n" + credentialScope + "\n" + GetHash(canonicalRequest)

	hash := GetHMAC([]byte(fmt.Sprintf("AWS4%s", resSecretMatch)), []byte(datestamp))
	hash = GetHMAC(hash, []byte(region))
	hash = GetHMAC(hash, []byte(service))
	hash = GetHMAC(hash, []byte("aws4_request"))

	signature2 := GetHMAC(hash, []byte(stringToSign)) // Get Signature HMAC SHA256
	signature := hex.EncodeToString(signature2)

	authorizationHeader := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, resIDMatch, credentialScope, signedHeaders, signature)

	req.Header.Add("Authorization", authorizationHeader)
	req.Header.Add("x-amz-date", amzDate)
	req.Header.Add("x-amz-security-token", resSessionMatch)

	req.URL.RawQuery = params.Encode()

	client := s.verificationClient
	if client == nil {
		client = defaultVerificationClient
	}

	extraData := map[string]string{
		"rotation_guide": "https://howtorotate.com/docs/tutorials/aws/",
	}

	res, err := client.Do(req)
	if err == nil {
		defer res.Body.Close()
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			identityInfo := identityRes{}
			err := json.NewDecoder(res.Body).Decode(&identityInfo)
			if err == nil {
				extraData["account"] = identityInfo.GetCallerIdentityResponse.GetCallerIdentityResult.Account
				extraData["user_id"] = identityInfo.GetCallerIdentityResponse.GetCallerIdentityResult.UserID
				extraData["arn"] = identityInfo.GetCallerIdentityResponse.GetCallerIdentityResult.Arn
				return true, extraData, nil
			} else {
				return false, nil, err
			}
		} else if res.StatusCode == 403 {
			// Experimentation has indicated that if you make two GetCallerIdentity requests within five seconds that
			// share a key ID but are signed with different secrets the second one will be rejected with a 403 that
			// carries a SignatureDoesNotMatch code in its body. This happens even if the second ID-secret pair is
			// valid. Since this is exactly our access pattern, we need to work around it.
			//
			// Fortunately, experimentation has also revealed a workaround: simply resubmit the second request. The
			// response to the resubmission will be as expected. But there's a caveat: You can't have closed the body of
			// the response to the original second request, or read to its end, or the resubmission will also yield a
			// SignatureDoesNotMatch. For this reason, we have to re-request all 403s. We can't re-request only
			// SignatureDoesNotMatch responses, because we can only tell whether a given 403 is a SignatureDoesNotMatch
			// after decoding its response body, which requires reading the entire response body, which disables the
			// workaround.
			//
			// We are clearly deep in the guts of AWS implementation details here, so this all might change with no
			// notice. If you're here because something in this detector broke, you have my condolences.
			if retryOn403 {
				return s.verifyMatch(ctx, resIDMatch, resSecretMatch, resSessionMatch, false)
			}
			var body awsErrorResponseBody
			err = json.NewDecoder(res.Body).Decode(&body)
			if err == nil {
				// All instances of the code I've seen in the wild are PascalCased but this check is
				// case-insensitive out of an abundance of caution
				if strings.EqualFold(body.Error.Code, "InvalidClientTokenId") {
					return false, nil, nil
				} else {
					return false, nil, fmt.Errorf("request to %v returned status %d with an unexpected reason (%s: %s)", res.Request.URL, res.StatusCode, body.Error.Code, body.Error.Message)
				}
			} else {
				return false, nil, fmt.Errorf("couldn't parse the sts response body (%v)", err)
			}
		} else {
			return false, nil, fmt.Errorf("request to %v returned unexpected status %d", res.Request.URL, res.StatusCode)
		}
	} else {
		return false, nil, err
	}
}

func awsCustomCleanResults(results []detectors.Result) []detectors.Result {
	if len(results) == 0 {
		return results
	}

	// For every ID, we want at most one result, preferably verified.
	idResults := map[string]detectors.Result{}
	for _, result := range results {
		// Always accept the verified result as the result for the given ID.
		if result.Verified {
			idResults[result.Redacted] = result
			continue
		}

		// Only include an unverified result if we don't already have a result for a given ID.
		if _, exist := idResults[result.Redacted]; !exist {
			idResults[result.Redacted] = result
		}
	}

	var out []detectors.Result
	for _, r := range idResults {
		out = append(out, r)
	}
	return out
}

type awsError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

type awsErrorResponseBody struct {
	Error awsError `json:"Error"`
}

type identityRes struct {
	GetCallerIdentityResponse struct {
		GetCallerIdentityResult struct {
			Account string `json:"Account"`
			Arn     string `json:"Arn"`
			UserID  string `json:"UserId"`
		} `json:"GetCallerIdentityResult"`
		ResponseMetadata struct {
			RequestID string `json:"RequestId"`
		} `json:"ResponseMetadata"`
	} `json:"GetCallerIdentityResponse"`
}

func (s scanner) Type() detectorspb.DetectorType {
	return detectorspb.DetectorType_AWSSessionKey
}
