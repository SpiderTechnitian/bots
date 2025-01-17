// Package trivia ...
package trivia

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

const baseURL = "https://opentdb.com/"

type response struct {
	ResponseCode int `json:"response_code"`
	Results      []struct {
		Category         string   `json:"category"`
		Type             string   `json:"type"`
		Difficulty       string   `json:"difficulty"`
		Question         string   `json:"question"`
		CorrectAnswer    string   `json:"correct_answer"`
		IncorrectAnswers []string `json:"incorrect_answers"`
	} `json:"results"`
}

type Round struct {
	logger       *zap.SugaredLogger
	Category     string
	Difficulty   string
	Question     string
	Type         string
	Answers      []*Answer
	Participants []*Participant
	Complete     bool
	Num          int
	StartedAt    time.Time
	PrevRound    *Round
	NextRound    *Round
}

type Answer struct {
	Value   string
	Correct bool
}

type Participant struct {
	Name             string
	Choice           int
	TimeToSubmission time.Duration
}

type Quiz struct {
	logger       *zap.SugaredLogger
	client       *http.Client
	url          string
	duration     time.Duration
	FirstRound   *Round
	CurrentRound *Round
	Timer        *time.Timer
	InProgress   bool
	Scoreboard   map[string]int
}

func NewDefaultQuiz(logger *zap.SugaredLogger) (*Quiz, error) {
	return NewQuiz(logger, 3, 30*time.Second)
}

func NewQuiz(logger *zap.SugaredLogger, size int, duration time.Duration) (*Quiz, error) {
	rand.Seed(time.Now().UnixNano())

	client := &http.Client{}
	resp, err := client.Get(fmt.Sprintf("%s/api_token.php?command=request", baseURL))
	if err != nil {
		return nil, fmt.Errorf("failed to request token: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.Debug("server returned bad response", "status_code", resp.StatusCode)
		return nil, fmt.Errorf("server returned invalid http code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response body: %w", err)
	}

	tokenRes := struct {
		Token string `json:"token"`
	}{}

	if err = json.Unmarshal(body, &tokenRes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token: %w", err)
	}

	u, err := url.Parse(fmt.Sprintf("%s/api.php", baseURL))
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %w", err)
	}

	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query: %w", err)
	}

	q.Add("token", tokenRes.Token)
	q.Add("amount", fmt.Sprint(size))
	u.RawQuery = q.Encode()

	quiz := &Quiz{
		client:     client,
		url:        u.String(),
		duration:   duration,
		logger:     logger,
		InProgress: false,
		Scoreboard: map[string]int{},
	}

	quiz.logger.Info("new quiz created, creating new series of rounds")
	if err = quiz.newSeries(); err != nil {
		return nil, fmt.Errorf("error creating new quiz: %w", err)
	}

	return quiz, nil
}

func (q *Quiz) newSeries() error {
	resp, err := q.client.Get(q.url)
	if err != nil {
		return fmt.Errorf("failed to get api data: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		q.logger.Debugw("bad response from server", "response_code", resp.StatusCode)
		return fmt.Errorf("server returned bad http status: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read api response body: %w", err)
	}

	var resultsResp response
	if err = json.Unmarshal(body, &resultsResp); err != nil {
		return fmt.Errorf("failed to unmarshal api response body: %w", err)
	}

	if len(resultsResp.Results) == 0 {
		return fmt.Errorf("server returned no results: %v", resultsResp)
	}

	roundNum := 1
	var head *Round
	var curr *Round
	for _, result := range resultsResp.Results {
		round := &Round{
			logger:     q.logger,
			Category:   result.Category,
			Difficulty: result.Difficulty,
			Question:   result.Question,
			Type:       result.Type,
			Answers: []*Answer{
				{result.CorrectAnswer, true},
			},
			Num:          roundNum,
			PrevRound:    curr,
			NextRound:    nil,
			Complete:     false,
			Participants: []*Participant{},
		}

		for _, value := range result.IncorrectAnswers {
			round.Answers = append(round.Answers, &Answer{value, false})
		}

		if head == nil {
			head = round
		}

		if curr != nil {
			curr.NextRound = round
		}

		curr = round
		roundNum++
	}

	q.FirstRound = head

	return nil
}

func (q *Quiz) StartRound(
	onComplete func(string, []*Participant) error,
) (*Round, error) {
	q.logger.Info("starting round")

	if q.FirstRound == nil {
		return nil, fmt.Errorf("rounds are not initialized")
	}

	// find the first round in the list that is not marked as complete
	q.CurrentRound = q.FirstRound
	for q.CurrentRound.Complete {
		if tmp := q.CurrentRound; tmp.NextRound != nil {
			q.CurrentRound = tmp.NextRound
		}
	}

	q.logger.Infow("determined round...", "question", q.CurrentRound.Question)

	if q.CurrentRound.Type == "boolean" {
		for idx, answer := range q.CurrentRound.Answers {
			if strings.ToLower(answer.Value) == "true" && idx != 0 {
				q.CurrentRound.Answers[idx-1], q.CurrentRound.Answers[idx] = q.CurrentRound.Answers[idx], q.CurrentRound.Answers[idx-1]
				break
			}
		}
	} else {
		rand.Shuffle(len(q.CurrentRound.Answers), func(i, j int) {
			q.CurrentRound.Answers[i], q.CurrentRound.Answers[j] = q.CurrentRound.Answers[j], q.CurrentRound.Answers[i]
		})
	}

	q.Timer = time.AfterFunc(q.duration, func() {
		q.logger.Info("time is up!")

		// append onto the current quiz leaderboard
		score := 3
		winners, losers := q.CurrentRound.DetermineOutcome()
		for _, v := range winners {
			if score >= 1 {
				q.Scoreboard[v.Name] += score * 2
				score--
			} else {
				q.Scoreboard[v.Name] += 1
			}
		}

		for _, v := range losers {
			if _, ok := q.Scoreboard[v.Name]; !ok {
				q.Scoreboard[v.Name] = 0
			}
		}

		// determine correct answer and format it
		var correct string
		for idx, ans := range q.CurrentRound.Answers {
			if ans.Correct {
				correct = fmt.Sprintf("`%d) %s`", idx+1, ans.Value)
				break
			}
		}

		q.logger.Infof("the correct answer is %q", correct)

		if err := onComplete(correct, winners); err != nil {
			q.logger.Fatalf("failed to run onComplete: %v", err)
		}
		q.InProgress = false
		q.CurrentRound.Complete = true
	})

	q.logger.Infow("timer started, round set to in progress", "duration", q.duration)
	q.InProgress = true

	return q.CurrentRound, nil
}

func (q *Quiz) SortedScore() map[string]int {
	type score struct {
		name   string
		points int
	}

	var ss []score
	for k, v := range q.Scoreboard {
		ss = append(ss, score{k, v})
	}

	// sort winners by points for top 3
	sort.Slice(ss, func(i, j int) bool {
		return ss[i].points > ss[j].points
	})

	// update leaderboard at the end of the quiz with all users' points
	data := map[string]int{}
	for _, score := range ss {
		data[score.name] = score.points
	}
	return data
}

func (r *Round) NewParticipant(username string, answer int, timeIn int64) bool {
	for _, participant := range r.Participants {
		if participant.Name == username {
			return false
		}
	}

	if answer >= len(r.Answers) {
		return false
	}

	timeToSub := time.Unix(timeIn/1000, timeIn%1000*int64(time.Millisecond)).Sub(r.StartedAt)
	p := &Participant{username, answer, timeToSub}

	r.Participants = append(r.Participants, p)
	r.logger.Infow("new participant", "entry", p)

	return true
}

func (r *Round) DetermineOutcome() ([]*Participant, []*Participant) {
	correctIdx := 0
	for idx, ans := range r.Answers {
		if ans.Correct {
			correctIdx = idx
			break
		}
	}

	losers := []*Participant{}
	winners := []*Participant{}
	// filter participants for correct choice
	for _, participant := range r.Participants {
		if participant.Choice == correctIdx {
			winners = append(winners, participant)
		} else {
			losers = append(losers, participant)
		}
	}

	// sort participants by time in
	sort.Slice(winners, func(i, j int) bool {
		return winners[i].TimeToSubmission < winners[j].TimeToSubmission
	})

	r.logger.Infow("winners determined", "winners", winners)
	return winners, losers
}
