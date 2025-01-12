package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-github/v45/github"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/geoip2-golang"
	"github.com/oschwald/maxminddb-golang"
	"github.com/sagernet/sing-box/common/srs"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
)

var githubClient *github.Client

func init() {
	accessToken, loaded := os.LookupEnv("ACCESS_TOKEN")
	if !loaded {
		githubClient = github.NewClient(nil)
		return
	}
	transport := &github.BasicAuthTransport{
		Username: accessToken,
	}
	githubClient = github.NewClient(transport.Client())
}

func fetch(from string) (*github.RepositoryRelease, error) {
	fixedRelease := os.Getenv("FIXED_RELEASE")
	names := strings.SplitN(from, "/", 2)
	if fixedRelease != "" {
		latestRelease, _, err := githubClient.Repositories.GetReleaseByTag(context.Background(), names[0], names[1], fixedRelease)
		if err != nil {
			return nil, err
		}
		return latestRelease, err
	} else {
		latestRelease, _, err := githubClient.Repositories.GetLatestRelease(context.Background(), names[0], names[1])
		if err != nil {
			return nil, err
		}
		return latestRelease, err
	}
}

func get(downloadURL *string) ([]byte, error) {
	log.Info("download ", *downloadURL)
	response, err := http.Get(*downloadURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func download(release *github.RepositoryRelease) ([]byte, error) {
	geoipAsset := common.Find(release.Assets, func(it *github.ReleaseAsset) bool {
		return *it.Name == "Country.mmdb"
	})
	if geoipAsset == nil {
		return nil, E.New("Country.mmdb not found in upstream release ", release.Name)
	}
	return get(geoipAsset.BrowserDownloadURL)
}

func parse(binary []byte) (metadata maxminddb.Metadata, countryMap map[string][]*net.IPNet, err error) {
	database, err := maxminddb.FromBytes(binary)
	if err != nil {
		return
	}
	metadata = database.Metadata
	networks := database.Networks(maxminddb.SkipAliasedNetworks)
	countryMap = make(map[string][]*net.IPNet)
	var country geoip2.Enterprise
	var ipNet *net.IPNet
	for networks.Next() {
		ipNet, err = networks.Network(&country)
		if err != nil {
			return
		}
		code := strings.ToLower(country.RegisteredCountry.IsoCode)
		countryMap[code] = append(countryMap[code], ipNet)
	}
	err = networks.Err()
	return
}

func newWriter(metadata maxminddb.Metadata, codes []string) (*mmdbwriter.Tree, error) {
	return mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "geoip",
		Languages:               codes,
		IPVersion:               int(metadata.IPVersion),
		RecordSize:              int(metadata.RecordSize),
		Inserter:                inserter.ReplaceWith,
		DisableIPv4Aliasing:     true,
		IncludeReservedNetworks: true,
	})
}

func write(writer *mmdbwriter.Tree, dataMap map[string][]*net.IPNet, output string, codes []string) error {
	if len(codes) == 0 {
		codes = make([]string, 0, len(dataMap))
		for code := range dataMap {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	codeMap := make(map[string]bool)
	for _, code := range codes {
		codeMap[code] = true
	}
	for code, data := range dataMap {
		if !codeMap[code] {
			continue
		}
		for _, item := range data {
			err := writer.Insert(item, mmdbtype.String(code))
			if err != nil {
				return err
			}
		}
	}
	outputFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	_, err = writer.WriteTo(outputFile)
	return err
}

func fetchChinaIPCIDR() ([]*net.IPNet, error) {
	urls := []string{
		"https://raw.githubusercontent.com/misakaio/chnroutes2/master/chnroutes.txt",
		"https://raw.githubusercontent.com/gaoyifan/china-operator-ip/ip-lists/china6.txt",
	}

	var allCIDRs []*net.IPNet

	for _, url := range urls {
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				_, ipNet, err := net.ParseCIDR(line)
				if err != nil {
					log.Warn("Invalid CIDR:", line)
					continue
				}
				allCIDRs = append(allCIDRs, ipNet)
			}
		}

		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}

	return allCIDRs, nil
}

func writeCIDRsToFile(cidrs []*net.IPNet, outputDir string, countryCode string, format string) error {
	fileName := "geoip-" + countryCode + "." + format
	filePath, _ := filepath.Abs(filepath.Join(outputDir, fileName))
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	switch format {
	case "txt":
		for _, cidr := range cidrs {
			writer.WriteString(cidr.String() + "\n")
		}
	case "list":
		for _, cidr := range cidrs {
			if cidr.IP.To4() != nil {
				writer.WriteString("IP-CIDR," + cidr.String() + "\n")
			} else {
				writer.WriteString("IP-CIDR6," + cidr.String() + "\n")
			}
		}
	case "yaml":
		writer.WriteString("payload:\n")
		for _, cidr := range cidrs {
			writer.WriteString("  - " + cidr.String() + "\n")
		}
	case "snippet":
		for _, cidr := range cidrs {
			if cidr.IP.To4() != nil {
				writer.WriteString("ip-cidr, " + cidr.String() + "\n")
			} else {
				writer.WriteString("ip6-cidr, " + cidr.String() + "\n")
			}
		}
	}

	log.Info("write ", filePath)
	return nil
}

func release(source string, destination string, output string, ruleSetOutput string) error {
	sourceRelease, err := fetch(source)
	if err != nil {
		return err
	}
	destinationRelease, err := fetch(destination)
	if err != nil {
		log.Warn("missing destination latest release")
	} else {
		if os.Getenv("NO_SKIP") != "true" && strings.Contains(*destinationRelease.Name, *sourceRelease.Name) {
			log.Info("already latest")
			setActionOutput("skip", "true")
			return nil
		}
	}
	binary, err := download(sourceRelease)
	if err != nil {
		return err
	}
	metadata, countryMap, err := parse(binary)
	if err != nil {
		return err
	}

	// Fetch China IPCIDR from specified URLs
	chinaCIDRs, err := fetchChinaIPCIDR()
	if err != nil {
		return err
	}

	// Replace the original China IPCIDR with the new one
	countryMap["cn"] = chinaCIDRs

	allCodes := make([]string, 0, len(countryMap))
	for code := range countryMap {
		allCodes = append(allCodes, code)
	}

	writer, err := newWriter(metadata, allCodes)
	if err != nil {
		return err
	}
	err = write(writer, countryMap, output, nil)
	if err != nil {
		return err
	}

	writer, err = newWriter(metadata, []string{"cn"})
	if err != nil {
		return err
	}
	err = write(writer, countryMap, "geoip-cn.db", []string{"cn"})
	if err != nil {
		return err
	}

	os.RemoveAll(ruleSetOutput)
	err = os.MkdirAll(ruleSetOutput, 0o755)
	if err != nil {
		return err
	}

	for countryCode, ipNets := range countryMap {
		var headlessRule option.DefaultHeadlessRule
		headlessRule.IPCIDR = make([]string, 0, len(ipNets))
		for _, cidr := range ipNets {
			headlessRule.IPCIDR = append(headlessRule.IPCIDR, cidr.String())
		}
		var plainRuleSet option.PlainRuleSet
		plainRuleSet.Rules = []option.HeadlessRule{
			{
				Type:           C.RuleTypeDefault,
				DefaultOptions: headlessRule,
			},
		}

		// Generate SRS file
		srsPath, _ := filepath.Abs(filepath.Join(ruleSetOutput, "geoip-"+countryCode+".srs"))
		log.Info("write ", srsPath)
		outputRuleSet, err := os.Create(srsPath)
		if err != nil {
			return err
		}
		err = srs.Write(outputRuleSet, plainRuleSet)
		if err != nil {
			outputRuleSet.Close()
			return err
		}
		outputRuleSet.Close()

		// Generate additional file formats
		for _, format := range []string{"txt", "list", "yaml", "snippet"} {
			err = writeCIDRsToFile(ipNets, ruleSetOutput, countryCode, format)
			if err != nil {
				return err
			}
		}
	}

	setActionOutput("tag", *sourceRelease.Name)
	return nil
}

func setActionOutput(name string, content string) {
	os.Stdout.WriteString("::set-output name=" + name + "::" + content + "\n")
}

func main() {
	err := release("Dreamacro/maxmind-geoip", "caocaocc/geoip", "geoip.db", "rule-set")
	if err != nil {
		log.Fatal(err)
	}
}
