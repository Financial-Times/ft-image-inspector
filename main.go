package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html"
)

var (
	basicAuth   string = ""
	printOnly   bool   = false
	docStoreURL string = ""
	delayInMs   int    = 1000
	uuidFile    string = ""
	brokenFile  string = ""
)

type Content struct {
	UUID      string `json:"uuid"`
	MainImage string `json:"mainImage"`
	Type      string `json:"type"`
	Members   []struct {
		UUID string `json:"uuid"`
	} `json:"members"`
	Body             string `json:"body"`
	BodyXML          string `json:"bodyXML"`
	PublishReference string `json:"publishReference"`
}

func (c *Content) GetBody() string {
	if c.Body != "" {
		return c.Body
	}
	return c.BodyXML
}

func main() {
	flag.StringVar(&basicAuth, "auth", "", "base64 encoded auth for the delivery cluster")
	flag.BoolVar(&printOnly, "printonly", false, "do not check but only print article/image uuids")
	flag.StringVar(&docStoreURL, "docstoreurl", "", "url of the document store service")
	flag.IntVar(&delayInMs, "delay", 1000, "throttle delay in miliseconds")
	flag.StringVar(&uuidFile, "uuidfile", "", "json file that holds a list with the uuids to be verified")
	flag.StringVar(&brokenFile, "brokenfile", "", "file that will hold the uuid of the broken publications")
	flag.Parse()

	if len(basicAuth) == 0 {
		fmt.Print("parameter auth not provided. terminating...\n")
		os.Exit(-1)
	}

	fmt.Print("Starting...\n")
	if printOnly {
		fmt.Print("Printing only uuids without checking images\n")
	}

	data, err := loadUUIDList(uuidFile)
	if err != nil {
		fmt.Printf("unable to load uuid list %s", err)
		return
	}

	broken := []string{}
	for _, id := range data {
		err := checkContent(id)
		if !printOnly {
			if err != nil {
				fmt.Printf("broken: %s (%s)\n", id, err)
				broken = append(broken, id)
			} else {
				fmt.Printf("safe: %s\n", id)
			}
		}

		time.Sleep(time.Duration(delayInMs) * time.Millisecond)
	}

	if !printOnly && brokenFile != "" {
		broken = dedupStrings(broken)
		f, _ := os.Create(brokenFile)
		defer f.Close()

		_, err := f.WriteString(strings.Join(broken, "\n"))
		if err != nil {
			fmt.Printf("error: %v\n", err)
		}
	}

	fmt.Print("Finished!\n")
}

func loadUUIDList(fileName string) ([]string, error) {
	uuidFile, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer uuidFile.Close()

	uuidValues, err := ioutil.ReadAll(uuidFile)
	if err != nil {
		return nil, err
	}

	var uuids []string
	err = json.Unmarshal(uuidValues, &uuids)
	if err != nil {
		return nil, err
	}

	return uuids, nil
}

func checkContent(uuid string) error {
	c, err := getContentFromDocumentStore(uuid)
	if err != nil {
		if printOnly {
			fmt.Printf("unable to find content with %s in the document-store\n", uuid)
		}
		return err
	}

	if (!printOnly) && (!strings.Contains(c.PublishReference, "tid_methode_carousel_")) {
		return fmt.Errorf("content %s not published by the upp-methode-converter", uuid)
	}

	switch c.Type {
	case "Image", "Graphic":
		if printOnly {
			fmt.Println(uuid)
		}
		return nil //Being able to load the content with the correct tid is OK
	case "ImageSet":
		return checkImageSet(c)
	case "Article":
		if printOnly {
			fmt.Println(uuid)
		}
		return checkArticle(c)
	default:
		return fmt.Errorf("error: %s unexpected type %s", uuid, c.Type)
	}
}

func checkArticle(c *Content) error {
	imageSets, err := getImageSetFromBody(c)
	if err != nil {
		return err
	}

	imageSets = dedupStrings(imageSets)
	for _, imgSet := range imageSets {
		err = checkContent(imgSet)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkImageSet(c *Content) error {
	for _, member := range c.Members {
		if c.UUID == member.UUID {
			if printOnly {
				continue
			} else {
				return fmt.Errorf("cycle reference detected in image set %s", c.UUID)
			}
		}

		err := checkContent(member.UUID)
		if err != nil {
			return err
		}
	}

	return nil
}

func getContentFromDocumentStore(uuid string) (*Content, error) {
	url := docStoreURL + uuid
	method := "GET"

	client := &http.Client{}
	req, err := http.NewRequest(method, url, nil)

	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Basic "+basicAuth)
	req.Header.Add("X-Request-Id", "tid_ftimageinspector_"+uuid)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("error %d", res.StatusCode)
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var c Content
	err = json.Unmarshal(body, &c)
	return &c, err
}

func getImageSetFromBody(c *Content) ([]string, error) {
	bodyAsString := c.GetBody()
	if bodyAsString == "" {
		return nil, nil
	}

	bodyReader := strings.NewReader(bodyAsString)
	htmlDoc, err := html.Parse(bodyReader)
	if err != nil {
		return nil, err
	}

	images := collectImageSets(htmlDoc)
	return images, nil
}

func collectImageSets(node *html.Node) []string {
	var imageSets []string

	if node.Type == html.ElementNode && (node.Data == "ft-content" || node.Data == "content") {
		for _, attr := range node.Attr {
			if attr.Key == "type" && attr.Val == "http://www.ft.com/ontology/content/ImageSet" {
				imageSets = parseImageSets(imageSets, node)
			}
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		imgs := collectImageSets(child)
		imageSets = append(imageSets, imgs...)
	}

	return imageSets
}

func parseImageSets(images []string, node *html.Node) []string {
	urlAttr := findNodeAttributeByKey(node.Attr, "url")
	if urlAttr != nil {
		imageUUID := extractUUIDfromURL(urlAttr.Val)
		images = append(images, imageUUID)
	}

	idAttr := findNodeAttributeByKey(node.Attr, "id")
	if idAttr != nil {
		images = append(images, idAttr.Val)
	}

	return images
}

func findNodeAttributeByKey(attr []html.Attribute, key string) *html.Attribute {
	for _, a := range attr {
		if a.Key == key {
			return &a
		}
	}

	return nil
}

func extractUUIDfromURL(URL string) string {
	items := strings.Split(URL, "/")
	return items[len(items)-1]
}

func dedupStrings(uuids []string) []string {
	resutl := []string{}
	set := map[string]bool{}
	for _, id := range uuids {
		set[id] = true
	}
	for key := range set {
		resutl = append(resutl, key)
	}
	return resutl
}
