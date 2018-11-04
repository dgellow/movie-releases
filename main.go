package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	telegram "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/pkg/errors"
)

var subscribeCommand = regexp.MustCompile("subscribe to (.+)")

func main() {
	botKey := os.Getenv("TELEGRAM_BOT_KEY")
	movieAPIKey := os.Getenv("THEMOVIEDB_API_KEY")

	bot, err := telegram.NewBotAPI(botKey)
	if err != nil {
		log.Fatalf("failed to create bot: %s", err)
	}

	bot.Debug = true

	log.Printf("Authorized on account %s", bot.Self.UserName)

	_, err = bot.SetWebhook(telegram.NewWebhook("https://6e8148f7.ngrok.io/" + bot.Token))
	if err != nil {
		log.Fatalf("failed to setup webhook: %s", err)
	}

	info, err := bot.GetWebhookInfo()
	if err != nil {
		log.Fatalf("failed to get webhook info: %s", err)
	}
	if info.LastErrorDate != 0 {
		log.Printf("telegram callback failed: %s", info.LastErrorMessage)
	}

	updates := bot.ListenForWebhook("/" + bot.Token)
	go http.ListenAndServe(":8080", nil)

	subscriptionsMap := make(map[int64][]subscription)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.Text == "" {
			continue
		}

		matches := subscribeCommand.FindStringSubmatch(strings.ToLower(update.Message.Text))
		if len(matches) != 2 {
			continue
		}

		movieTitle := strings.ToLower(matches[1])

		results, err := queryMovies(movieAPIKey, movieTitle)
		if err != nil {
			log.Fatalf("failed to search movies: %s", err)
		}
		sort.Sort(sort.Reverse(results))

		switch len(results) {
		case 0:
			sendMsg(bot, telegram.NewMessage(update.Message.Chat.ID, "Nothing found :("))
		case 1:
			subs := subscriptionsMap[update.Message.Chat.ID]
			found := false
			for i := range subs {
				if subs[i].title == movieTitle {
					found = true
					break
				}
			}
			if !found {
				subs = append(subs, subscription{
					releaseDate: time.Now().Add(5 * time.Hour),
					title:       movieTitle,
				})
			}
			subscriptionsMap[update.Message.Chat.ID] = subs

			text := fmt.Sprintf("Current subscriptions: %+v", subscriptionsMap[update.Message.Chat.ID])
			sendMsg(bot, telegram.NewMessage(update.Message.Chat.ID, text))
		default:
			text := "Found more than one match:\n"
			for _, m := range results {
				year := fmt.Sprintf("%d", m.ReleaseTime.Year())
				if m.ReleaseTime.IsZero() {
					year = "unknown release date"
				}
				text += fmt.Sprintf("- %s (%s)\n", m.Title, year)
			}
			sendMsg(bot, telegram.NewMessage(update.Message.Chat.ID, text))
		}
	}
}

func sendMsg(bot *telegram.BotAPI, msg telegram.MessageConfig) {
	if _, err := bot.Send(msg); err != nil {
		log.Fatalf("failed to send message: %s", err)
	}
}

type subscription struct {
	releaseDate time.Time
	title       string
}

type MovieAPIResult struct {
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	ID          int64  `json:"id"`
	ReleaseTime time.Time
}

type MovieAPIResults []MovieAPIResult

func (r MovieAPIResults) Len() int           { return len(r) }
func (r MovieAPIResults) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r MovieAPIResults) Less(i, j int) bool { return r[i].ReleaseTime.Before(r[j].ReleaseTime) }

func queryMovies(apiKey, movieTitle string) (MovieAPIResults, error) {
	u, err := url.Parse(fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s", apiKey))
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse url")
	}
	q := u.Query()
	q.Set("query", movieTitle)
	u.RawQuery = q.Encode()

	res, err := http.Get(u.String())
	if err != nil {
		return nil, errors.Wrap(err, "failed to send http get request")
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.Errorf("unexpected status code: %d", res.StatusCode)
	}

	var data struct {
		Results []MovieAPIResult `json:"results"`
	}

	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed read request body")
	}

	if err := json.Unmarshal(b, &data); err != nil {
		return nil, errors.Wrap(err, "failed to parse json")
	}

	for i := range data.Results {
		if data.Results[i].ReleaseDate == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", data.Results[i].ReleaseDate)
		data.Results[i].ReleaseTime = t
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse release date")
		}
	}

	return data.Results, nil
}
