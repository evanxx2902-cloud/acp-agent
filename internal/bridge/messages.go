package bridge

import (
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/schema"
)

// ContentBlocksToMessage converts ACP ContentBlocks to a single eino user Message.
func ContentBlocksToMessage(blocks []acp.ContentBlock) *schema.Message {
	var textParts []string
	var imageParts []schema.MessageInputPart

	for _, block := range blocks {
		switch {
		case block.Text != nil:
			textParts = append(textParts, block.Text.Text)
		case block.Image != nil:
			imageParts = append(imageParts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{
						Base64Data: &block.Image.Data,
						MIMEType:   block.Image.MimeType,
					},
				},
			})
		case block.ResourceLink != nil:
			textParts = append(textParts,
				fmt.Sprintf("[Resource: %s](%s)", block.ResourceLink.Name, block.ResourceLink.Uri))
		case block.Resource != nil:
			if block.Resource.Resource.TextResourceContents != nil {
				textParts = append(textParts, block.Resource.Resource.TextResourceContents.Text)
			}
		case block.Audio != nil:
			textParts = append(textParts, "[Audio content attached]")
		}
	}

	textContent := strings.Join(textParts, "\n")

	if len(imageParts) > 0 {
		// Multimodal message
		parts := make([]schema.MessageInputPart, 0, len(imageParts)+1)
		if textContent != "" {
			parts = append(parts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeText,
				Text: textContent,
			})
		}
		parts = append(parts, imageParts...)
		return &schema.Message{
			Role:                schema.User,
			UserInputMultiContent: parts,
		}
	}

	return schema.UserMessage(textContent)
}

