package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	telegram "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/pkg/errors"
)

const region = "DE"

var (
	regionToEmoji = map[string]string{
		"DE": "🇩🇪",
	}

	subscribeCommand         = regexp.MustCompile("subscribe to (.+)")
	releaseCommand           = regexp.MustCompile("releases? ?(exact)? (.+)")
	releaseYearCommand       = regexp.MustCompile("releases? ?(exact)? (.+) year ([0-9]{4})")
	listSubscriptionsCommand = regexp.MustCompile("list subscriptions?")

	movieAPIKey     = ""
	datastoreClient *datastore.Client
	bot             *telegram.BotAPI
)

func main() {
	host := os.Getenv("HOST")
	port := os.Getenv("PORT")
	botKey := os.Getenv("TELEGRAM_BOT_KEY")
	movieAPIKey = os.Getenv("THEMOVIEDB_API_KEY")

	// Create GCP datastore client
	ctx := context.TODO()
	var err error
	datastoreClient, err = datastore.NewClient(ctx, "")
	if err != nil {
		log.Fatalf("failed to create datastore client: %s", err)
	}

	// Create telegram bot API client
	bot, err = telegram.NewBotAPI(botKey)
	if err != nil {
		log.Fatalf("failed to create bot: %s", err)
	}

	bot.Debug = true

	log.Printf("Authorized on account %s", bot.Self.UserName)

	// Register telegram bot
	_, err = bot.SetWebhook(telegram.NewWebhook(host + "/" + bot.Token))
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

	// Listen for messages received by the bot
	updates := bot.ListenForWebhook("/" + bot.Token)

	// Listen for trigger of notify task
	http.HandleFunc("/tasks/notify", handleTaskNotify)

	go http.ListenAndServe(fmt.Sprintf(":%s", port), nil)

	// Handle bot messages
	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.Text == "" {
			continue
		}

		text := strings.TrimSpace(strings.ToLower(update.Message.Text))

		if matches := releaseYearCommand.FindStringSubmatch(text); matches != nil {
			handleRelease(update, matches)
		} else if matches := releaseCommand.FindStringSubmatch(text); matches != nil {
			handleRelease(update, matches)
		} else if matches := subscribeCommand.FindStringSubmatch(text); matches != nil {
			handleSubscribe(update, matches)
		} else if matches := listSubscriptionsCommand.FindStringSubmatch(text); matches != nil {
			handlelistSubscriptions(update)
		} else {
			msgText := "Looking for information about movie releases? I can help with the following questions 😌\n" +
				"`releases [exact] <movie title>`\n" +
				"`releases [exact] <movie title> year <year of release>` (the year of release can be region specific)\n" +
				"`subscribe to <movie title>`\n" +
				"`list subscriptions` (the year of release can be region specific)\n" +
				"\n" +
				"Examples:\n" +
				"`release climax year 2018`\n" +
				"`release exact julia`\n" +
				"`subscribe to Alita`\n" +
				"\n"

			regionEmoji, ok := regionToEmoji[region]
			if !ok {
				regionEmoji = region
			}

			msgText += "Current region: " + regionEmoji

			msgConfig := telegram.NewMessage(update.Message.Chat.ID, msgText)
			msgConfig.ParseMode = "Markdown"
			sendMsg(msgConfig)
		}
	}
}

func handleRelease(update telegram.Update, matches []string) {
	exact := false
	if matches[1] != "" {
		exact = true
	}

	title := matches[2]

	var year string
	if len(matches) == 4 {
		year = matches[3]
	}

	results, err := queryMovies(title, year)
	if err != nil {
		log.Fatalf("failed to search movies with year: %s", err)
	}

	if exact {
		for i := 0; i < len(results); i++ {
			if !strings.Contains(strings.ToLower(results[i].Title), title) {
				results = append(results[:i], results[i+1:]...)
				i--
			}
		}
	}

	sendResults(update, results)
}

func sendResults(update telegram.Update, results MovieAPIResults) {
	switch len(results) {
	case 0:
		sendMsg(telegram.NewMessage(update.Message.Chat.ID, "No entry found 🤓"))
	default:
		text := "I found these entries 🍿:\n"
		for _, m := range results {
			year := fmt.Sprintf("%d", m.ReleaseTime.Year())
			if m.ReleaseTime.IsZero() {
				year = "unknown release date"
			}
			text += fmt.Sprintf("- %s (%s)\n", m.Title, year)
		}
		sendMsg(telegram.NewMessage(update.Message.Chat.ID, text))
	}
}

func handleSubscribe(update telegram.Update, matches []string) {
	movieTitle := matches[1]
	results, err := queryMovies(movieTitle, "")
	if err != nil {
		log.Fatalf("failed to search movies with year: %s", err)
	}

	now := time.Now()

	var upcoming []MovieRelease
	for _, res := range results {
		if res.ReleaseTime.After(now) {
			upcoming = append(upcoming, MovieRelease{
				ID:          res.ID,
				MovieTitle:  res.Title,
				ReleaseDate: res.ReleaseTime,
			})
		}
	}

	var text string
	switch len(upcoming) {
	case 0:
		text = "No movie releases found :("
	case 1:
		release := upcoming[0]

		ctx := context.TODO()
		key := datastore.NameKey("MovieRelease", fmt.Sprintf("%d", release.ID), nil)
		_, err := datastoreClient.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
			var txRelease MovieRelease

			// Try to get a stored record
			err := tx.Get(key, &txRelease)
			if err != nil && err != datastore.ErrNoSuchEntity {
				return err
			}

			// Handle case where record doesn't exist yet
			if err == datastore.ErrNoSuchEntity {
				txRelease = release
			}

			// Create subscriber
			sub := Subscriber{
				Notified: false,
				ChatID:   update.Message.Chat.ID,
			}

			// Check if user already subscribed to movie release
			for i := range txRelease.Subscribers {
				if txRelease.Subscribers[i].ChatID == sub.ChatID {
					// user found, do not update
					return nil
				}
			}

			txRelease.Subscribers = append(txRelease.Subscribers, sub)

			_, err = tx.Put(key, &txRelease)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			log.Fatalf("failed to subscribe to movie release: %s", err)
		}

		text = "Done!"
	default:
		text = "Found multiple movies, be more specific please."
	}

	sendMsg(telegram.NewMessage(update.Message.Chat.ID, text))
}

func handlelistSubscriptions(update telegram.Update) {
	var records []MovieRelease
	_, err := datastoreClient.GetAll(context.TODO(), datastore.NewQuery("MovieRelease"), &records)
	if err != nil {
		log.Fatalf("failed to get all subscriptions: %s", err)
	}

	var subscriptions []MovieRelease
	for _, rec := range records {
		for _, sub := range rec.Subscribers {
			if sub.ChatID == update.Message.Chat.ID {
				subscriptions = append(subscriptions, rec)
				break
			}
		}
	}

	var text string
	switch len(subscriptions) {
	case 0:
		text = "No subscriptions found"
	default:
		text = "Your subscriptions are \n"
		for _, sub := range subscriptions {
			date := sub.ReleaseDate.Format("2 Jan 2006")
			text += fmt.Sprintf("- %s %s\n", sub.MovieTitle, date)
		}
	}
	sendMsg(telegram.NewMessage(update.Message.Chat.ID, text))
}

func sendMsg(msg telegram.MessageConfig) {
	if _, err := bot.Send(msg); err != nil {
		log.Fatalf("failed to send message: %s", err)
	}
}

const (
	// EntityMovieReleases ...
	EntityMovieReleases = "MovieReleases"
)

// Subscriber ...
type Subscriber struct {
	Notified bool
	ChatID   int64
}

// MovieRelease ...
type MovieRelease struct {
	ID          int64
	MovieTitle  string
	ReleaseDate time.Time
	Subscribers []Subscriber
}

// MovieAPIResult ...
type MovieAPIResult struct {
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	ID          int64  `json:"id"`
	ReleaseTime time.Time
}

// MovieAPIResults ...
type MovieAPIResults []MovieAPIResult

func (r MovieAPIResults) Len() int           { return len(r) }
func (r MovieAPIResults) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r MovieAPIResults) Less(i, j int) bool { return r[i].ReleaseTime.Before(r[j].ReleaseTime) }

func queryMovies(movieTitle, year string) (MovieAPIResults, error) {
	u, err := url.Parse(fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s", movieAPIKey))
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse url")
	}
	q := u.Query()
	q.Set("query", movieTitle)
	q.Set("year", year)
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
		Results MovieAPIResults `json:"results"`
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
	sort.Sort(sort.Reverse(data.Results))

	return data.Results, nil
}

func handleTaskNotify(w http.ResponseWriter, r *http.Request) {
	var records []MovieRelease
	keys, err := datastoreClient.GetAll(context.TODO(), datastore.NewQuery("MovieRelease"), &records)
	if err != nil {
		log.Fatalf("failed to get all subscriptions: %s", err)
	}

	for idxRecord, record := range records {
		now := time.Now()
		inOneWeek := now.Add(7 * 24 * time.Hour)
		if !(record.ReleaseDate.After(now) && record.ReleaseDate.Before(inOneWeek)) {
			continue
		}

		for idxSub, sub := range record.Subscribers {
			if sub.Notified {
				continue
			}

			days := int(math.Ceil(record.ReleaseDate.Sub(now).Hours() / 24))
			text := fmt.Sprintf("%s will be released in %d days.", record.MovieTitle, days)
			sendMsg(telegram.NewMessage(sub.ChatID, text))

			record.Subscribers[idxSub].Notified = true
		}

		key := keys[idxRecord]
		_, err = datastoreClient.Put(context.TODO(), key, &record)
		if err != nil {
			log.Fatalf("failed to update movie release: key=%v", key)
		}
	}
}
