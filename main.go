package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	basicAuth   string = ""
	printOnly   bool   = false
	docStoreURL string = ""
	delayInMs   int    = 1000
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
	flag.BoolVar(&printOnly, "printOnly", false, "do not check but only print article/image uuids")
	flag.StringVar(&docStoreURL, "docStoreURL", "", "url of the document store service")
	flag.IntVar(&delayInMs, "delay", 1000, "throttle delay in miliseconds")
	flag.Parse()

	if len(basicAuth) == 0 {
		fmt.Print("parameter auth not provided. terminating...\n")
		os.Exit(-1)
	}

	fmt.Print("Starting...\n")
	if printOnly {
		fmt.Print("Printing only uuids without checking images\n")
	}

	results := map[string][]string{}
	broken := []string{}
	for _, id := range data {
		if printOnly {
			fmt.Println(id)
		}
		images, err := getImagesForContent(id)
		if err != nil {
			fmt.Printf("%s - %s\n", err, id)
			continue
		}
		if images == nil {
			continue
		}

		results[id] = dedupStrings(images)

		for _, imageUUID := range results[id] {
			if printOnly && imageUUID != "" {
				fmt.Println(imageUUID)
			} else {
				img, err := checkImage(imageUUID)
				if err != nil {
					if errors.Is(err, ErrImageSetBroken) {
						fmt.Printf("broken: image: %s from image-set: %s article: %s\n", imageUUID, img.UUID, id)
						broken = append(broken, imageUUID)
						continue
					}
					if errors.Is(err, ErrContentNotPublishedByConverter) {
						fmt.Printf("content not published by upp-methode-converter. image: %s from image-set %s article: %s\n", imageUUID, img.UUID, id)
						broken = append(broken, imageUUID)
						continue
					}
					fmt.Printf("error: %v\n", err)
					continue
				}
				fmt.Printf("safe: %s\n", imageUUID)
			}
		}
		time.Sleep(time.Duration(delayInMs) * time.Millisecond)
	}

	if !printOnly {
		broken = dedupStrings(broken)
		f, _ := os.Create("broken-images")
		defer f.Close()

		_, err := f.WriteString(strings.Join(broken, "\n"))
		if err != nil {
			fmt.Printf("error: %v\n", err)
		}
	}

	fmt.Print("Finished!\n")
}

var (
	ErrImageSetBroken                 = errors.New("image set broken")
	ErrContentNotPublishedByConverter = errors.New("content not published by the upp-methode-converter")
)

func checkImage(uuid string) (*Content, error) {
	c, err := getContentFromDocumentStore(uuid)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case "Image", "ImageSet", "Graphic":
		break
	default:
		return nil, fmt.Errorf("error: %s unexpected type %s", uuid, c.Type)
	}

	if !strings.Contains(c.PublishReference, "tid_methode_carousel_") {
		return c, ErrContentNotPublishedByConverter
	}

	// if its not Image Set probably not broken
	if c.Type != "ImageSet" {
		return c, nil
	}
	// if it has more than one members - not broken
	if len(c.Members) != 1 {
		return c, nil
	}
	memberUUID := c.Members[0].UUID
	m, err := getContentFromDocumentStore(memberUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member '%s' for '%s': %w", memberUUID, uuid, err)
	}
	for _, inM := range m.Members {
		if inM.UUID == memberUUID {
			return m, ErrImageSetBroken
		}
	}
	// nothing broken in this ImageSet
	return c, nil
}

func getImagesForContent(uuid string) ([]string, error) {
	c, err := getContentFromDocumentStore(uuid)
	if err != nil {
		//fmt.Printf("error: %v\n", err)
		return nil, err
	}
	if c.Type != "Article" {
		return nil, nil
	}

	images, err := getImageSetFromBody(c)
	if err != nil {
		fmt.Printf("body img error: %v\n", err)
		return nil, err
	}
	images = append(images, c.MainImage)
	return images, nil
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
	//using ParseFragment since Parse will construct a well-formed HTML by introducing <HTML> element
	htmlDoc, err := html.ParseFragment(bodyReader, &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		return nil, err
	}

	images := collectImageSets(htmlDoc)
	return images, nil
}

func collectImageSets(nodeList []*html.Node) []string {
	var images []string
	for _, node := range nodeList {
		if node.Type == html.ElementNode && (node.Data == "ft-content" || node.Data == "content") {
			hasImageSet := false
			for _, attr := range node.Attr {
				if attr.Key == "type" && attr.Val == "http://www.ft.com/ontology/content/ImageSet" {
					hasImageSet = true
					break
				}
			}
			for _, attr := range node.Attr {
				if !hasImageSet {
					break
				}
				if attr.Key == "url" {
					imageUUID := extractUUIDfromURL(attr.Val)
					images = append(images, imageUUID)
					break
				}
				if attr.Key == "id" {
					images = append(images, attr.Val)
					break
				}
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			imgs := collectImageSets([]*html.Node{child})
			images = append(images, imgs...)
		}
	}
	return images
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
