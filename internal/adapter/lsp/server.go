package lsp

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"time"

	"github.com/mickael-menu/zk/internal/core"
	"github.com/mickael-menu/zk/internal/util"
	dateutil "github.com/mickael-menu/zk/internal/util/date"
	"github.com/mickael-menu/zk/internal/util/errors"
	"github.com/mickael-menu/zk/internal/util/opt"
	strutil "github.com/mickael-menu/zk/internal/util/strings"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	glspserv "github.com/tliron/glsp/server"
	"github.com/tliron/kutil/logging"
	_ "github.com/tliron/kutil/logging/simple"
)

// Server holds the state of the Language Server.
type Server struct {
	server         *glspserv.Server
	notebooks      *core.NotebookStore
	documents      *documentStore
	templateLoader core.TemplateLoader
	fs             core.FileStorage
	logger         util.Logger
}

// ServerOpts holds the options to create a new Server.
type ServerOpts struct {
	Name           string
	Version        string
	LogFile        opt.String
	Logger         *util.ProxyLogger
	Notebooks      *core.NotebookStore
	TemplateLoader core.TemplateLoader
	FS             core.FileStorage
}

// NewServer creates a new Server instance.
func NewServer(opts ServerOpts) *Server {
	fs := opts.FS
	debug := !opts.LogFile.IsNull()
	if debug {
		logging.Configure(10, opts.LogFile.Value)
	}

	handler := protocol.Handler{}
	glspServer := glspserv.NewServer(&handler, opts.Name, debug)

	// Redirect zk's logger to GLSP's to avoid breaking the JSON-RPC protocol
	// with unwanted output.
	if opts.Logger != nil {
		opts.Logger.Logger = newGlspLogger(glspServer.Log)
	}

	server := &Server{
		server:         glspServer,
		notebooks:      opts.Notebooks,
		documents:      newDocumentStore(fs, opts.Logger),
		templateLoader: opts.TemplateLoader,
		fs:             fs,
		logger:         opts.Logger,
	}

	var clientCapabilities protocol.ClientCapabilities

	handler.Initialize = func(context *glsp.Context, params *protocol.InitializeParams) (interface{}, error) {
		clientCapabilities = params.Capabilities

		// To see the logs with coc.nvim, run :CocCommand workspace.showOutput
		// https://github.com/neoclide/coc.nvim/wiki/Debug-language-server#using-output-channel
		if params.Trace != nil {
			protocol.SetTraceValue(*params.Trace)
		}

		capabilities := handler.CreateServerCapabilities()
		capabilities.HoverProvider = true
		capabilities.DefinitionProvider = true
		capabilities.CodeActionProvider = true

		change := protocol.TextDocumentSyncKindIncremental
		capabilities.TextDocumentSync = protocol.TextDocumentSyncOptions{
			OpenClose: boolPtr(true),
			Change:    &change,
			Save:      boolPtr(true),
		}
		capabilities.DocumentLinkProvider = &protocol.DocumentLinkOptions{
			ResolveProvider: boolPtr(true),
		}

		triggerChars := []string{"(", "[", "#", ":"}

		capabilities.ExecuteCommandProvider = &protocol.ExecuteCommandOptions{
			Commands: []string{
				cmdIndex,
				cmdNew,
			},
		}
		capabilities.CompletionProvider = &protocol.CompletionOptions{
			TriggerCharacters: triggerChars,
			ResolveProvider:   boolPtr(true),
		}

		capabilities.ReferencesProvider = &protocol.ReferenceOptions{}

		return protocol.InitializeResult{
			Capabilities: capabilities,
			ServerInfo: &protocol.InitializeResultServerInfo{
				Name:    opts.Name,
				Version: &opts.Version,
			},
		}, nil
	}

	handler.Initialized = func(context *glsp.Context, params *protocol.InitializedParams) error {
		return nil
	}

	handler.Shutdown = func(context *glsp.Context) error {
		protocol.SetTraceValue(protocol.TraceValueOff)
		return nil
	}

	handler.SetTrace = func(context *glsp.Context, params *protocol.SetTraceParams) error {
		protocol.SetTraceValue(params.Value)
		return nil
	}

	handler.TextDocumentDidOpen = func(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
		doc, err := server.documents.DidOpen(*params, context.Notify)
		if err != nil {
			return err
		}
		if doc != nil {
			server.refreshDiagnosticsOfDocument(doc, context.Notify, false)
		}
		return nil
	}

	handler.TextDocumentDidChange = func(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil
		}

		doc.ApplyChanges(params.ContentChanges)
		server.refreshDiagnosticsOfDocument(doc, context.Notify, true)
		return nil
	}

	handler.TextDocumentDidClose = func(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
		server.documents.Close(params.TextDocument.URI)
		return nil
	}

	handler.TextDocumentDidSave = func(context *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			server.logger.Err(err)
			return nil
		}

		_, err = notebook.Index(false)
		server.logger.Err(err)
		return nil
	}

	handler.TextDocumentCompletion = func(context *glsp.Context, params *protocol.CompletionParams) (interface{}, error) {
		// We don't use the context because clients might not send it. Instead,
		// we'll look for trigger patterns in the document.
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		switch doc.LookBehind(params.Position, 3) {
		case "]((":
			return server.buildLinkCompletionList(doc, notebook, params)
		}

		switch doc.LookBehind(params.Position, 2) {
		case "[[":
			return server.buildLinkCompletionList(doc, notebook, params)
		}

		switch doc.LookBehind(params.Position, 1) {
		case "#":
			if notebook.Config.Format.Markdown.Hashtags {
				return server.buildTagCompletionList(notebook, "#")
			}
		case ":":
			if notebook.Config.Format.Markdown.ColonTags {
				return server.buildTagCompletionList(notebook, ":")
			}
		}

		return nil, nil
	}

	handler.CompletionItemResolve = func(context *glsp.Context, params *protocol.CompletionItem) (*protocol.CompletionItem, error) {
		if path, ok := params.Data.(string); ok {
			content, err := ioutil.ReadFile(path)
			if err != nil {
				return params, err
			}
			params.Documentation = protocol.MarkupContent{
				Kind:  protocol.MarkupKindMarkdown,
				Value: string(content),
			}
		}

		return params, nil
	}

	handler.TextDocumentHover = func(context *glsp.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}

		link, err := doc.DocumentLinkAt(params.Position)
		if link == nil || err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		target, err := server.noteForLink(*link, doc, notebook)
		if err != nil || target == nil {
			return nil, err
		}

		path, err := uriToPath(target.URI)
		if err != nil {
			server.logger.Printf("unable to parse URI: %v", err)
			return nil, err
		}
		path = fs.Canonical(path)

		contents, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		return &protocol.Hover{
			Contents: protocol.MarkupContent{
				Kind:  protocol.MarkupKindMarkdown,
				Value: string(contents),
			},
		}, nil
	}

	handler.TextDocumentDocumentLink = func(context *glsp.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}

		links, err := doc.DocumentLinks()
		if err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		documentLinks := []protocol.DocumentLink{}
		for _, link := range links {
			target, err := server.noteForLink(link, doc, notebook)
			if target == nil || err != nil {
				continue
			}

			documentLinks = append(documentLinks, protocol.DocumentLink{
				Range:  link.Range,
				Target: &target.URI,
			})
		}

		return documentLinks, err
	}

	handler.TextDocumentDefinition = func(context *glsp.Context, params *protocol.DefinitionParams) (interface{}, error) {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}

		link, err := doc.DocumentLinkAt(params.Position)
		if link == nil || err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		target, err := server.noteForLink(*link, doc, notebook)
		if link == nil || target == nil || err != nil {
			return nil, err
		}

		// FIXME: Waiting for https://github.com/tliron/glsp/pull/3 to be
		// merged before using LocationLink.
		if false && isTrue(clientCapabilities.TextDocument.Definition.LinkSupport) {
			return protocol.LocationLink{
				OriginSelectionRange: &link.Range,
				TargetURI:            target.URI,
			}, nil
		} else {
			return protocol.Location{
				URI: target.URI,
			}, nil
		}
	}

	handler.WorkspaceExecuteCommand = func(context *glsp.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
		switch params.Command {
		case cmdIndex:
			return server.executeCommandIndex(params.Arguments)
		case cmdNew:
			return server.executeCommandNew(context, params.Arguments)
		default:
			return nil, fmt.Errorf("unknown zk LSP command: %s", params.Command)
		}
	}

	handler.TextDocumentCodeAction = func(context *glsp.Context, params *protocol.CodeActionParams) (interface{}, error) {
		if isRangeEmpty(params.Range) {
			return nil, nil
		}

		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}
		wd := filepath.Dir(doc.Path)

		actions := []protocol.CodeAction{}

		addAction := func(dir string, actionTitle string) error {
			opts := cmdNewOpts{
				Title: doc.ContentAtRange(params.Range),
				Dir:   dir,
				InsertLinkAtLocation: &protocol.Location{
					URI:   params.TextDocument.URI,
					Range: params.Range,
				},
			}

			var jsonOpts map[string]interface{}
			err := unmarshalJSON(opts, &jsonOpts)
			if err != nil {
				return err
			}

			actions = append(actions, protocol.CodeAction{
				Title: actionTitle,
				Kind:  stringPtr(protocol.CodeActionKindRefactor),
				Command: &protocol.Command{
					Command:   cmdNew,
					Arguments: []interface{}{wd, jsonOpts},
				},
			})

			return nil
		}

		addAction(wd, "New note in current directory")
		addAction("", "New note in top directory")

		return actions, nil
	}

	handler.TextDocumentReferences = func(context *glsp.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
		doc, ok := server.documents.Get(params.TextDocument.URI)
		if !ok {
			return nil, nil
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		link, err := doc.DocumentLinkAt(params.Position)
		if err != nil {
			return nil, err
		}
		if link == nil {
			href, err := notebook.RelPath(doc.Path)
			if err != nil {
				return nil, err
			}
			link = &documentLink{Href: href}
		}

		target, err := server.noteForLink(*link, doc, notebook)
		if link == nil || target == nil || err != nil {
			return nil, err
		}

		p, err := notebook.RelPath(target.Path)
		if err != nil {
			return nil, err
		}

		opts := core.NoteFindOpts{
			LinkTo: &core.LinkFilter{Paths: []string{p}},
		}

		notes, err := notebook.FindNotes(opts)
		if err != nil {
			return nil, err
		}

		var locations []protocol.Location

		for _, note := range notes {
			pos := strings.Index(note.RawContent, target.Path[0:len(target.Path)-3])
			var line uint32 = 0
			if pos < 0 {
				line = 0
			} else {
				linePos := strings.Count(note.RawContent[0:pos], "\n")
				line = uint32(linePos)
			}

			locations = append(locations, protocol.Location{
				URI: pathToURI(filepath.Join(notebook.Path, note.Path)),
				Range: protocol.Range{
					Start: protocol.Position{
						Line:      line,
						Character: 0,
					},
					End: protocol.Position{
						Line:      line,
						Character: 0,
					},
				},
			})
		}

		return locations, nil
	}

	return server
}

// Run starts the Language Server in stdio mode.
func (s *Server) Run() error {
	return errors.Wrap(s.server.RunStdio(), "lsp")
}

const cmdIndex = "zk.index"

func (s *Server) executeCommandIndex(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("zk.index expects a notebook path as first argument")
	}
	path, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("zk.index expects a notebook path as first argument, got: %v", args[0])
	}

	force := false
	if len(args) == 2 {
		options, ok := args[1].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("zk.index expects a dictionary of options as second argument, got: %v", args[1])
		}
		if forceOption, ok := options["force"]; ok {
			force = toBool(forceOption)
		}
	}

	notebook, err := s.notebooks.Open(path)
	if err != nil {
		return nil, err
	}

	return notebook.Index(force)
}

const cmdNew = "zk.new"

type cmdNewOpts struct {
	Title                string             `json:"title,omitempty"`
	Content              string             `json:"content,omitempty"`
	Dir                  string             `json:"dir,omitempty"`
	Group                string             `json:"group,omitempty"`
	Template             string             `json:"template,omitempty"`
	Extra                map[string]string  `json:"extra,omitempty"`
	Date                 string             `json:"date,omitempty"`
	Edit                 jsonBoolean        `json:"edit,omitempty"`
	InsertLinkAtLocation *protocol.Location `json:"insertLinkAtLocation,omitempty"`
}

func (s *Server) executeCommandNew(context *glsp.Context, args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("zk.index expects a notebook path as first argument")
	}
	wd, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("zk.index expects a notebook path as first argument, got: %v", args[0])
	}

	var opts cmdNewOpts
	if len(args) > 1 {
		arg, ok := args[1].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("zk.new expects a dictionary of options as second argument, got: %v", args[1])
		}
		err := unmarshalJSON(arg, &opts)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse zk.new args, got: %v", arg)
		}
	}

	notebook, err := s.notebooks.Open(wd)
	if err != nil {
		return nil, err
	}

	date, err := dateutil.TimeFromNatural(opts.Date)
	if err != nil {
		return nil, errors.Wrapf(err, "%s, failed to parse the `date` option", opts.Date)
	}

	note, err := notebook.NewNote(core.NewNoteOpts{
		Title:     opt.NewNotEmptyString(opts.Title),
		Content:   opts.Content,
		Directory: opt.NewNotEmptyString(opts.Dir),
		Group:     opt.NewNotEmptyString(opts.Group),
		Template:  opt.NewNotEmptyString(opts.Template),
		Extra:     opts.Extra,
		Date:      date,
	})
	if err != nil {
		var noteExists core.ErrNoteExists
		if !errors.As(err, &noteExists) {
			return nil, err
		}
		note, err = notebook.FindNote(core.NoteFindOpts{
			IncludePaths: []string{noteExists.Name},
		})
		if err != nil {
			return nil, err
		}
	}
	if note == nil {
		return nil, errors.New("zk.new could not generate a new note")
	}

	if opts.InsertLinkAtLocation != nil {
		doc, ok := s.documents.Get(opts.InsertLinkAtLocation.URI)
		if !ok {
			return nil, fmt.Errorf("can't insert link in %s", opts.InsertLinkAtLocation.URI)
		}
		linkFormatter, err := notebook.NewLinkFormatter()
		if err != nil {
			return nil, err
		}

		currentDir := filepath.Dir(doc.Path)
		linkFormatterContext, err := core.NewLinkFormatterContext(note.AsMinimalNote(), notebook.Path, currentDir)
		if err != nil {
			return nil, err
		}

		link, err := linkFormatter(linkFormatterContext)
		if err != nil {
			return nil, err
		}

		go context.Call(protocol.ServerWorkspaceApplyEdit, protocol.ApplyWorkspaceEditParams{
			Edit: protocol.WorkspaceEdit{
				Changes: map[string][]protocol.TextEdit{
					opts.InsertLinkAtLocation.URI: {{Range: opts.InsertLinkAtLocation.Range, NewText: link}},
				},
			},
		}, nil)
	}

	absPath := filepath.Join(notebook.Path, note.Path)
	if opts.Edit {
		go context.Call(protocol.ServerWindowShowDocument, protocol.ShowDocumentParams{
			URI:       pathToURI(absPath),
			TakeFocus: boolPtr(true),
		}, nil)
	}

	return map[string]interface{}{"path": absPath}, nil
}

func (s *Server) notebookOf(doc *document) (*core.Notebook, error) {
	return s.notebooks.Open(doc.Path)
}

// noteForLink returns the LSP documentUri for the note targeted by the given link.
//
// Match by order of precedence:
//  1. Prefix of relative path
//  2. Find any occurrence of the href in a note path (substring)
//  3. Match the href as a term in the note titles
func (s *Server) noteForLink(link documentLink, doc *document, notebook *core.Notebook) (*Note, error) {
	note, err := s.noteForHref(link.Href, doc, notebook)
	if note == nil && err == nil && link.IsWikiLink {
		// Try to find a partial href match.
		note, err = notebook.FindByHref(link.Href, true)
		if note == nil && err == nil {
			// Fallback on matching the note title.
			note, err = s.noteMatchingTitle(link.Href, notebook)
		}
	}
	if note == nil || err != nil {
		return nil, err
	}

	joined_path := filepath.Join(notebook.Path, note.Path)
	return &Note{*note, pathToURI(joined_path)}, nil
}

// noteForHref returns the LSP documentUri for the note targeted by the given HREF.
func (s *Server) noteForHref(href string, doc *document, notebook *core.Notebook) (*core.MinimalNote, error) {
	if strutil.IsURL(href) {
		return nil, nil
	}

	path := filepath.Clean(filepath.Join(filepath.Dir(doc.Path), href))
	path, err := filepath.Rel(notebook.Path, path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve href: %s", href)
	}
	note, err := notebook.FindByHref(path, false)
	if err != nil {
		s.logger.Printf("findByHref(%s): %s", href, err.Error())
	}
	return note, err
}

// noteMatchingTitle returns the LSP documentUri for the note matching the given search terms.
func (s *Server) noteMatchingTitle(terms string, notebook *core.Notebook) (*core.MinimalNote, error) {
	if terms == "" {
		return nil, nil
	}

	note, err := notebook.FindMatching("title:(" + terms + ")")
	if err != nil {
		s.logger.Printf("findMatching(title: %s): %s", terms, err.Error())
	}
	return note, err
}

type Note struct {
	core.MinimalNote
	URI protocol.DocumentUri
}

func (s *Server) refreshDiagnosticsOfDocument(doc *document, notify glsp.NotifyFunc, delay bool) {
	if doc.NeedsRefreshDiagnostics { // Already refreshing
		return
	}

	notebook, err := s.notebookOf(doc)
	if err != nil {
		s.logger.Err(err)
		return
	}

	diagConfig := notebook.Config.LSP.Diagnostics
	if diagConfig.WikiTitle == core.LSPDiagnosticNone && diagConfig.DeadLink == core.LSPDiagnosticNone {
		// No diagnostic enabled.
		return
	}

	doc.NeedsRefreshDiagnostics = true
	go func() {
		if delay {
			time.Sleep(1 * time.Second)
		}
		doc.NeedsRefreshDiagnostics = false

		diagnostics := []protocol.Diagnostic{}
		links, err := doc.DocumentLinks()
		if err != nil {
			s.logger.Err(err)
			return
		}

		for _, link := range links {
			if strutil.IsURL(link.Href) {
				continue
			}
			target, err := s.noteForLink(link, doc, notebook)
			if err != nil {
				s.logger.Err(err)
				continue
			}

			var severity protocol.DiagnosticSeverity
			var message string
			if target == nil {
				if diagConfig.DeadLink == core.LSPDiagnosticNone {
					continue
				}
				severity = protocol.DiagnosticSeverity(diagConfig.DeadLink)
				message = "not found"
			} else {
				if link.HasTitle || diagConfig.WikiTitle == core.LSPDiagnosticNone {
					continue
				}
				severity = protocol.DiagnosticSeverity(diagConfig.WikiTitle)
				message = target.Title
			}

			diagnostics = append(diagnostics, protocol.Diagnostic{
				Range:    link.Range,
				Severity: &severity,
				Source:   stringPtr("zk"),
				Message:  message,
			})
		}

		go notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
			URI:         doc.URI,
			Diagnostics: diagnostics,
		})
	}()
}

func (s *Server) buildTagCompletionList(notebook *core.Notebook, triggerChar string) ([]protocol.CompletionItem, error) {
	tags, err := notebook.FindCollections(core.CollectionKindTag, nil)
	if err != nil {
		return nil, err
	}

	var items []protocol.CompletionItem
	for _, tag := range tags {
		items = append(items, protocol.CompletionItem{
			Label:      tag.Name,
			InsertText: s.buildInsertForTag(tag.Name, triggerChar, notebook.Config),
			Detail:     stringPtr(fmt.Sprintf("%d %s", tag.NoteCount, strutil.Pluralize("note", tag.NoteCount))),
		})
	}

	return items, nil
}

func (s *Server) buildInsertForTag(name string, triggerChar string, config core.Config) *string {
	switch triggerChar {
	case ":":
		name += ":"
	case "#":
		if strings.Contains(name, " ") {
			if config.Format.Markdown.MultiwordTags {
				name += "#"
			} else {
				name = strings.ReplaceAll(name, " ", "\\ ")
			}
		}
	}
	return &name
}

func (s *Server) buildLinkCompletionList(doc *document, notebook *core.Notebook, params *protocol.CompletionParams) ([]protocol.CompletionItem, error) {
	linkFormatter, err := newLinkFormatter(doc, notebook, params)
	if err != nil {
		return nil, err
	}

	templates, err := newCompletionTemplates(s.templateLoader, notebook.Config.LSP.Completion.Note)
	if err != nil {
		return nil, err
	}

	notes, err := notebook.FindMinimalNotes(core.NoteFindOpts{})
	if err != nil {
		return nil, err
	}

	var items []protocol.CompletionItem
	for _, note := range notes {
		item, err := s.newCompletionItem(notebook, note, doc, params.Position, linkFormatter, templates)
		if err != nil {
			s.logger.Err(err)
			continue
		}

		items = append(items, item)
	}

	return items, nil
}

func newLinkFormatter(doc *document, notebook *core.Notebook, params *protocol.CompletionParams) (core.LinkFormatter, error) {
	if doc.LookBehind(params.Position, 3) == "]((" {
		return core.NewMarkdownLinkFormatter(notebook.Config.Format.Markdown, true)
	} else {
		return notebook.NewLinkFormatter()
	}
}

func (s *Server) newCompletionItem(notebook *core.Notebook, note core.MinimalNote, doc *document, pos protocol.Position, linkFormatter core.LinkFormatter, templates completionTemplates) (protocol.CompletionItem, error) {
	kind := protocol.CompletionItemKindReference
	item := protocol.CompletionItem{
		Kind: &kind,
		Data: filepath.Join(notebook.Path, note.Path),
	}

	templateContext, err := newCompletionItemRenderContext(note, notebook.Path, doc.Path)
	if err != nil {
		return item, err
	}

	if templates.Label != nil {
		item.Label, err = templates.Label.Render(templateContext)
		if err != nil {
			return item, err
		}
	} else {
		item.Label = note.Title
	}
	// Fallback on the note path to never have empty labels.
	if item.Label == "" {
		item.Label = note.Path
	}

	if templates.FilterText != nil {
		filterText, err := templates.FilterText.Render(templateContext)
		if err != nil {
			return item, err
		}
		item.FilterText = &filterText
	}
	if item.FilterText == nil || *item.FilterText == "" {
		// Add the path to the filter text to be able to complete by it.
		item.FilterText = stringPtr(item.Label + " " + note.Path)
	}

	if templates.Detail != nil {
		detail, err := templates.Detail.Render(templateContext)
		if err != nil {
			return item, err
		}
		item.Detail = &detail
	}

	item.TextEdit, err = s.newTextEditForLink(notebook, note, doc, pos, linkFormatter)
	if err != nil {
		err = errors.Wrapf(err, "failed to build TextEdit for note at %s", note.Path)
		return item, err
	}

	addTextEdits := []protocol.TextEdit{}

	// Some LSP clients (e.g. VSCode) don't support deleting the trigger
	// characters with the main TextEdit. So let's add an additional
	// TextEdit for that.
	addTextEdits = append(addTextEdits, protocol.TextEdit{
		NewText: "",
		Range:   rangeFromPosition(pos, -2, 0),
	})

	item.AdditionalTextEdits = addTextEdits

	return item, nil
}

func (s *Server) newTextEditForLink(notebook *core.Notebook, note core.MinimalNote, doc *document, pos protocol.Position, linkFormatter core.LinkFormatter) (interface{}, error) {
	currentDir := filepath.Dir(doc.Path)
	context, err := core.NewLinkFormatterContext(note, notebook.Path, currentDir)
	if err != nil {
		return nil, err
	}
	link, err := linkFormatter(context)
	if err != nil {
		return nil, err
	}

	// Some LSP clients (e.g. VSCode) auto-pair brackets, so we need to
	// remove the closing ]] or )) after the completion.
	endOffset := 0
	suffix := doc.LookForward(pos, 2)
	if suffix == "]]" || suffix == "))" {
		endOffset = 2
	}

	return protocol.TextEdit{
		NewText: link,
		Range:   rangeFromPosition(pos, 0, endOffset),
	}, nil
}

func positionInRange(content string, rng protocol.Range, pos protocol.Position) bool {
	start, end := rng.IndexesIn(content)
	i := pos.IndexIn(content)
	return i >= start && i <= end
}

func rangeFromPosition(pos protocol.Position, startOffset, endOffset int) protocol.Range {
	offsetPos := func(offset int) protocol.Position {
		newPos := pos
		if offset < 0 {
			newPos.Character -= uint32(-offset)
		} else {
			newPos.Character += uint32(offset)
		}
		return newPos
	}

	return protocol.Range{
		Start: offsetPos(startOffset),
		End:   offsetPos(endOffset),
	}
}

func isRangeEmpty(pos protocol.Range) bool {
	return pos.Start == pos.End
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func isTrue(v *bool) bool {
	return v != nil && *v == true
}

func isFalse(v *bool) bool {
	return v == nil || *v == false
}

func stringPtr(v string) *string {
	s := v
	return &s
}

func unmarshalJSON(obj interface{}, v interface{}) error {
	js, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return json.Unmarshal(js, v)
}

func toBool(obj interface{}) bool {
	s := strings.ToLower(fmt.Sprint(obj))
	return s == "true" || s == "1"
}
