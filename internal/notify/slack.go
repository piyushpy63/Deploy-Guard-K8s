package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type SlackNotifier struct {
	WebhookURL string
	HTTPClient *http.Client
}

type slackMessage struct {
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	Color  string  `json:"color"`
	Fields []field `json:"fields"`
}

type field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

func NewSlackNotifier(webhookURL string) *SlackNotifier {
	return &SlackNotifier{
		WebhookURL: webhookURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *SlackNotifier) Send(namespace, deployment, verdict string, score float64, reasons string, dryRun bool) error {
	// Choose color and emoji based on verdict
	color := "good"
	emoji := "✅"

	switch verdict {
	case "WARN":
		color = "warning"
		emoji = "⚠️"
	case "ROLLBACK":
		color = "danger"
		emoji = "🚨"
	}

	// Build title
	title := fmt.Sprintf("%s Deploy Guard: *%s* for `%s/%s`",
		emoji, verdict, namespace, deployment,
	)

	if dryRun {
		title += " _(dry-run)_"
	}

	msg := slackMessage{
		Text: title,
		Attachments: []attachment{
			{
				Color: color,
				Fields: []field{
					{
						Title: "Score",
						Value: fmt.Sprintf("%.2f / 1.00", score),
						Short: true,
					},
					{
						Title: "Namespace",
						Value: namespace,
						Short: true,
					},
					{
						Title: "Deployment",
						Value: deployment,
						Short: true,
					},
					{
						Title: "Time",
						Value: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
						Short: true,
					},
					{
						Title: "Reasons",
						Value: reasons,
						Short: false,
					},
				},
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal slack message: %w", err)
	}

	resp, err := s.HTTPClient.Post(
		s.WebhookURL,
		"application/json",
		bytes.NewBuffer(data),
	)
	if err != nil {
		return fmt.Errorf("failed to send slack message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack returned non-200 status: %d", resp.StatusCode)
	}

	return nil
}
