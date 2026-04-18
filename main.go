package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"
)

const (
	githubAPI       = "https://api.github.com/repos/NotiLo-A/foxbot/contents/foxes"
	githubRaw       = "https://raw.githubusercontent.com/NotiLo-A/foxbot/main/foxes/"
	pollTimeout     = 30
	refreshInterval = time.Hour
	workers         = 4
)

type githubFile struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type cache struct {
	mu  sync.RWMutex
	img []string
}

func (c *cache) refresh(client *http.Client) error {
	req, err := http.NewRequest("GET", githubAPI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api: status %d", resp.StatusCode)
	}

	var files []githubFile
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return err
	}

	imgs := make([]string, 0, len(files))
	for _, f := range files {
		if f.Type == "file" {
			imgs = append(imgs, githubRaw+f.Name)
		}
	}

	if len(imgs) == 0 {
		return fmt.Errorf("no files found")
	}

	c.mu.Lock()
	c.img = imgs
	c.mu.Unlock()
	log.Printf("cache refreshed: %d images", len(imgs))
	return nil
}

func (c *cache) random() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.img) == 0 {
		return "", false
	}
	return c.img[rand.IntN(len(c.img))], true
}

type tgUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type tgResponse struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

type bot struct {
	token  string
	apiURL string
	client *http.Client
	imgs   *cache
}

func newBot(token string) *bot {
	return &bot{
		token:  token,
		apiURL: "https://api.telegram.org/bot" + token + "/",
		client: &http.Client{Timeout: (pollTimeout + 10) * time.Second},
		imgs:   &cache{},
	}
}

func (b *bot) call(method string, params url.Values) error {
	resp, err := b.client.PostForm(b.apiURL+method, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}

func (b *bot) getUpdates(offset int64) ([]tgUpdate, error) {
	resp, err := b.client.PostForm(b.apiURL+"getUpdates", url.Values{
		"offset":            {fmt.Sprint(offset)},
		"timeout":           {fmt.Sprint(pollTimeout)},
		"allowed_updates[]": {"message"},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r tgResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Result, nil
}

func (b *bot) sendFox(chatID int64) {
	imgURL, ok := b.imgs.random()
	if !ok {
		if err := b.call("sendMessage", url.Values{
			"chat_id": {fmt.Sprint(chatID)},
			"text":    {"🦊 no images cached yet, try again"},
		}); err != nil {
			log.Printf("sendMessage error chat=%d: %v", chatID, err)
		}
		return
	}

	imgResp, err := b.client.Get(imgURL)
	if err != nil {
		log.Printf("fetch image error: %v", err)
		return
	}
	defer imgResp.Body.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("chat_id", fmt.Sprint(chatID))
	fw, err := mw.CreateFormFile("photo", path.Base(imgURL))
	if err != nil {
		log.Printf("multipart error: %v", err)
		return
	}
	if _, err := io.Copy(fw, imgResp.Body); err != nil {
		log.Printf("copy error: %v", err)
		return
	}
	mw.Close()

	resp, err := b.client.Post(b.apiURL+"sendPhoto", mw.FormDataContentType(), &buf)
	if err != nil {
		log.Printf("sendPhoto error chat=%d: %v", chatID, err)
		return
	}
	defer resp.Body.Close()
	var res struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if !res.OK {
		log.Printf("sendPhoto error chat=%d: telegram: %s", chatID, res.Description)
	}
}

func main() {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN not set")
	}

	b := newBot(token)

	if err := b.imgs.refresh(b.client); err != nil {
		log.Fatalf("initial cache fetch failed: %v", err)
	}

	go func() {
		t := time.NewTicker(refreshInterval)
		for range t.C {
			if err := b.imgs.refresh(b.client); err != nil {
				log.Printf("cache refresh error: %v", err)
			}
		}
	}()

	jobs := make(chan tgUpdate, 64)
	for range workers {
		go func() {
			for u := range jobs {
				if u.Message != nil && u.Message.Text == "/fox" {
					b.sendFox(u.Message.Chat.ID)
				}
			}
		}()
	}

	log.Println("bot running")
	var offset int64
	for {
		updates, err := b.getUpdates(offset)
		if err != nil {
			log.Printf("poll error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = int64(u.UpdateID) + 1
			jobs <- u
		}
	}
}