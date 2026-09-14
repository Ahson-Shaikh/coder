package chatd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/x/chatd/chatprompt"
	"github.com/coder/coder/v2/coderd/x/chatd/chattool"
	"github.com/coder/coder/v2/codersdk"
)

const (
	memoryExtractionWorkTimeout        = 120 * time.Second
	memoryExtractionModelTimeout       = 60 * time.Second
	memoryExtractionTranscriptMaxBytes = 24 * 1024
	memoryExtractionMaxOutputTokens    = 2048
)

// The extractor is deliberately upsert-only. Dogfooding showed a model
// treating an assistant's "I don't know" as a contradiction and deleting a
// correct memory; deletion stays with the main agent's tool and the UI.
const memoryExtractionPrompt = "You review a completed coding-chat turn and record durable memory the main agent did not save itself. " +
	"%s " +
	chattool.MemoryGuidance + " " +
	"Record only facts the user stated or explicitly confirmed in this turn. " +
	"Never record that something is unknown, unspecified, undecided, or pending, and never record questions or the assistant's own guesses. " +
	"Skip anything already covered by a memory in the index; existing memories are updated by the main agent, not by you. " +
	"Most turns contain nothing new: return an empty list in that case."

type memoryExtraction struct {
	Upserts []memoryExtractionUpsert `json:"upserts"`
}

type memoryExtractionUpsert struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// resolveMemoryScope returns the durable-memory store available to a chat.
func (p *Server) resolveMemoryScope(ctx context.Context, chat database.Chat) (chattool.MemoryStore, chattool.MemoryScope, bool) {
	if chat.ParentChatID.Valid {
		return nil, chattool.MemoryScope{}, false
	}
	if chat.ProjectID.Valid {
		scope := chattool.MemoryScope{Kind: chattool.MemoryScopeProject}
		project, err := p.db.GetChatProjectByID(ctx, chat.ProjectID.UUID)
		if err != nil {
			p.logger.Debug(ctx, "failed to load chat project for memory scope", slog.F("chat_id", chat.ID), slog.Error(err))
		} else {
			scope.Label = project.Name
		}
		return chattool.NewProjectMemoryStore(p.db, chat.ProjectID.UUID, chat.OrganizationID, chat.ID, chat.OwnerID), scope, true
	}
	enabled, err := p.configCache.GetUserChatPersonalMemoryEnabled(ctx, chat.OwnerID)
	if err != nil {
		p.logger.Debug(ctx, "failed to load personal memory setting", slog.F("chat_id", chat.ID), slog.Error(err))
		return nil, chattool.MemoryScope{}, false
	}
	if !enabled {
		return nil, chattool.MemoryScope{}, false
	}
	return chattool.NewPersonalMemoryStore(p.db, chat.OwnerID, chat.OrganizationID, chat.ID), chattool.MemoryScope{Kind: chattool.MemoryScopePersonal}, true
}

func (p *Server) maybeExtractMemoriesAsync(ctx context.Context, logger slog.Logger, chat database.Chat) {
	if chat.ParentChatID.Valid {
		return
	}
	extractCtx, cancel := p.inflightContext(ctx)
	if err := p.goInflight(func() {
		defer cancel()
		p.extractMemories(extractCtx, logger, chat)
	}); err != nil {
		cancel()
		logger.Debug(ctx, "skipped memory extraction", slog.F("chat_id", chat.ID), slog.Error(err))
	}
}

func (p *Server) extractMemories(ctx context.Context, logger slog.Logger, chat database.Chat) {
	ctx, cancel := context.WithTimeout(ctx, memoryExtractionWorkTimeout)
	defer cancel()
	//nolint:gocritic // Background memory extraction acts as the chat daemon.
	ctx = dbauthz.AsChatd(ctx)

	chat, err := p.db.GetChatByID(ctx, chat.ID)
	if err != nil {
		logger.Debug(ctx, "failed to re-read chat for memory extraction", slog.Error(err))
		return
	}
	store, scope, ok := p.resolveMemoryScope(ctx, chat)
	if !ok {
		return
	}
	cursor, err := p.db.GetChatMemoryCursor(ctx, chat.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logger.Debug(ctx, "failed to read memory cursor", slog.F("chat_id", chat.ID), slog.Error(err))
		return
	}
	// A missing cursor leaves HistoryVersion at 0, which admits every message.
	if err == nil && chat.HistoryVersion <= cursor.HistoryVersion {
		return
	}

	messages, err := p.db.GetChatMessagesForPromptByChatID(ctx, chat.ID)
	if err != nil {
		logger.Debug(ctx, "failed to load memory transcript", slog.F("chat_id", chat.ID), slog.Error(err))
		return
	}
	transcript := renderMemoryTranscript(messages, cursor.HistoryVersion)
	if transcript == "" {
		return
	}
	if turnUsedMemoryTools(messages, cursor.HistoryVersion) {
		if _, err := p.db.UpsertChatMemoryCursor(ctx, database.UpsertChatMemoryCursorParams{ChatID: chat.ID, HistoryVersion: chat.HistoryVersion}); err != nil {
			logger.Debug(ctx, "failed to advance memory cursor", slog.F("chat_id", chat.ID), slog.Error(err))
		}
		return
	}
	entries, err := store.List(ctx)
	if err != nil {
		logger.Debug(ctx, "failed to load memories for extraction", slog.F("chat_id", chat.ID), slog.Error(err))
		return
	}

	apiKeyID, err := p.ensureSyntheticAPIKeyID(ctx, chat.OwnerID)
	if err != nil {
		logger.Debug(ctx, "failed to ensure synthetic API key for memory extraction", slog.Error(err))
		return
	}
	resolved, err := p.resolveModelCall(ctx, modelCallSpec{purpose: "memory_extraction", chat: chat, buildOptions: modelBuildOptions{ActiveAPIKeyID: apiKeyID}})
	if err != nil {
		logger.Debug(ctx, "failed to resolve model for memory extraction", slog.Error(err))
		return
	}
	call := resolved.newObjectCall("memory_extraction", "Record new durable memories stated by the user in this turn.", memoryExtractionMaxOutputTokens)
	call.Prompt = quickgenPrompt(fmt.Sprintf(memoryExtractionPrompt, scope.Intro()), fmt.Sprintf("Current memory index:\n%s\n\nNew user messages:\n%s", chattool.FormatMemoryIndex(scope, entries), transcript))
	modelCtx, cancelModel := context.WithTimeout(ctx, memoryExtractionModelTimeout)
	defer cancelModel()
	result, err := generateQuickgenObject[memoryExtraction](modelCtx, resolved.model.LanguageModel(), call)
	if err != nil {
		logger.Debug(ctx, "failed to generate memory extraction", slog.F("chat_id", chat.ID), slog.Error(err))
		return
	}
	for _, upsert := range result.Object.Upserts {
		if err := applyMemoryUpsert(ctx, store, upsert); err != nil {
			logger.Debug(ctx, "ignored invalid memory upsert", slog.F("chat_id", chat.ID), slog.F("name", upsert.Name), slog.Error(err))
		}
	}
	if _, err := p.db.UpsertChatMemoryCursor(ctx, database.UpsertChatMemoryCursorParams{ChatID: chat.ID, HistoryVersion: chat.HistoryVersion}); err != nil {
		logger.Debug(ctx, "failed to advance memory cursor", slog.F("chat_id", chat.ID), slog.Error(err))
	}
}

// applyMemoryUpsert records a memory the extractor proposed. It only creates:
// updates to existing memories are reserved for the main agent's tool and UI.
func applyMemoryUpsert(ctx context.Context, store chattool.MemoryStore, upsert memoryExtractionUpsert) error {
	input, err := normalizeMemoryExtraction(upsert)
	if err != nil {
		return err
	}
	_, existingErr := store.Get(ctx, input.Name)
	if existingErr == nil {
		return xerrors.Errorf("memory %q already exists", input.Name)
	}
	if !errors.Is(existingErr, chattool.ErrMemoryNotFound) {
		return xerrors.Errorf("look up memory: %w", existingErr)
	}
	count, err := store.Count(ctx)
	if err != nil {
		return xerrors.Errorf("count memories: %w", err)
	}
	if count >= chattool.MaxMemories {
		return xerrors.New("memory limit reached")
	}
	_, err = store.Upsert(ctx, input)
	return err
}

func normalizeMemoryExtraction(upsert memoryExtractionUpsert) (chattool.MemoryInput, error) {
	name := strings.ToLower(strings.TrimSpace(upsert.Name))
	if err := chattool.ValidateMemoryName(name); err != nil {
		return chattool.MemoryInput{}, err
	}
	description := chattool.NormalizeMemoryText(upsert.Description)
	body := chattool.NormalizeMemoryText(upsert.Body)
	if description == "" || len([]rune(description)) > chattool.MaxMemoryDescriptionChars {
		return chattool.MemoryInput{}, xerrors.New("invalid memory description")
	}
	if body == "" || len(body) > chattool.MaxMemoryBodyBytes {
		return chattool.MemoryInput{}, xerrors.New("invalid memory body")
	}
	return chattool.MemoryInput{Name: name, Description: description, Body: body}, nil
}

func turnUsedMemoryTools(messages []database.ChatMessage, afterHistoryVersion int64) bool {
	for _, message := range messages {
		if message.Revision <= afterHistoryVersion || message.Role != database.ChatMessageRoleAssistant {
			continue
		}
		parts, err := chatprompt.ParseContent(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == codersdk.ChatMessagePartTypeToolCall && (part.ToolName == chattool.SaveMemoryToolName || part.ToolName == chattool.DeleteMemoryToolName) {
				return true
			}
		}
	}
	return false
}

func renderMemoryTranscript(messages []database.ChatMessage, afterHistoryVersion int64) string {
	var lines []string
	for _, message := range messages {
		if message.Revision <= afterHistoryVersion || message.Role != database.ChatMessageRoleUser {
			continue
		}
		if message.Visibility != database.ChatMessageVisibilityBoth && message.Visibility != database.ChatMessageVisibilityUser {
			continue
		}
		parts, err := chatprompt.ParseContent(message)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(contentBlocksToText(parts))
		if text != "" {
			lines = append(lines, fmt.Sprintf("[%s]: %s", message.Role, text))
		}
	}
	for len(strings.Join(lines, "\n")) > memoryExtractionTranscriptMaxBytes && len(lines) > 1 {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}
