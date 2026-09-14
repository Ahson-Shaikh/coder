package chatd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbmock"
	"github.com/coder/coder/v2/coderd/x/chatd/chatprompt"
	"github.com/coder/coder/v2/coderd/x/chatd/chattool"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/quartz"
)

func TestRenderMemoryTranscript(t *testing.T) {
	t.Parallel()
	content, err := chatprompt.MarshalParts([]codersdk.ChatMessagePart{codersdk.ChatMessageText("new user detail")})
	require.NoError(t, err)
	transcript := renderMemoryTranscript([]database.ChatMessage{
		{Role: database.ChatMessageRoleUser, Visibility: database.ChatMessageVisibilityBoth, Revision: 3, Content: pqtype.NullRawMessage{RawMessage: content.RawMessage, Valid: true}, ContentVersion: chatprompt.CurrentContentVersion},
		{Role: database.ChatMessageRoleAssistant, Visibility: database.ChatMessageVisibilityBoth, Revision: 5, Content: pqtype.NullRawMessage{RawMessage: content.RawMessage, Valid: true}, ContentVersion: chatprompt.CurrentContentVersion},
		{Role: database.ChatMessageRoleUser, Visibility: database.ChatMessageVisibilityBoth, Revision: 5, Content: pqtype.NullRawMessage{RawMessage: content.RawMessage, Valid: true}, ContentVersion: chatprompt.CurrentContentVersion},
	}, 3)
	require.Contains(t, transcript, "new user detail")
}

func TestNormalizeMemoryExtraction(t *testing.T) {
	t.Parallel()
	normalized, err := normalizeMemoryExtraction(memoryExtractionUpsert{Name: "Release_Notes", Description: "<memory>Durable</memory>", Body: "<project-memory>Body</project-memory>"})
	require.NoError(t, err)
	require.Equal(t, chattool.MemoryInput{Name: "release_notes", Description: "Durable", Body: "Body"}, normalized)
}

func TestResolveMemoryScope(t *testing.T) {
	t.Parallel()
	t.Run("Personal", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		db := dbmock.NewMockStore(ctrl)
		userID := uuid.New()
		db.EXPECT().GetUserChatPersonalMemoryEnabled(gomock.Any(), userID).Return("true", nil)
		server := &Server{db: db, logger: slogtest.Make(t, nil), configCache: newChatConfigCache(t.Context(), db, quartz.NewReal())}
		_, scope, ok := server.resolveMemoryScope(t.Context(), database.Chat{ID: uuid.New(), OwnerID: userID, OrganizationID: uuid.New()})
		require.True(t, ok)
		require.Equal(t, chattool.MemoryScopePersonal, scope.Kind)
	})
	t.Run("ToggleOffSkipsModelCall", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		db := dbmock.NewMockStore(ctrl)
		chat := database.Chat{ID: uuid.New(), OwnerID: uuid.New(), OrganizationID: uuid.New(), HistoryVersion: 1}
		db.EXPECT().GetChatByID(gomock.Any(), chat.ID).Return(chat, nil)
		db.EXPECT().GetUserChatPersonalMemoryEnabled(gomock.Any(), chat.OwnerID).Return("false", nil)
		server := &Server{db: db, logger: slogtest.Make(t, nil), configCache: newChatConfigCache(t.Context(), db, quartz.NewReal())}
		server.extractMemories(t.Context(), slogtest.Make(t, nil), chat)
	})
	t.Run("Project", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		db := dbmock.NewMockStore(ctrl)
		projectID := uuid.New()
		db.EXPECT().GetChatProjectByID(gomock.Any(), projectID).Return(database.ChatProject{Name: "platform"}, nil)
		server := &Server{db: db, logger: slogtest.Make(t, nil)}
		_, scope, ok := server.resolveMemoryScope(context.Background(), database.Chat{ID: uuid.New(), OwnerID: uuid.New(), OrganizationID: uuid.New(), ProjectID: uuid.NullUUID{UUID: projectID, Valid: true}})
		require.True(t, ok)
		require.Equal(t, chattool.MemoryScopeProject, scope.Kind)
		require.Equal(t, "platform", scope.Label)
	})
	t.Run("PersonalAbsentDefaultsEnabled", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		db := dbmock.NewMockStore(ctrl)
		userID := uuid.New()
		db.EXPECT().GetUserChatPersonalMemoryEnabled(gomock.Any(), userID).Return("", sql.ErrNoRows)
		server := &Server{db: db, logger: slogtest.Make(t, nil), configCache: newChatConfigCache(t.Context(), db, quartz.NewReal())}
		_, _, ok := server.resolveMemoryScope(t.Context(), database.Chat{ID: uuid.New(), OwnerID: userID, OrganizationID: uuid.New()})
		require.True(t, ok)
	})
}
