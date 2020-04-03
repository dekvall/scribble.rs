package game

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	commands "github.com/Bios-Marcel/cmdp"
	"github.com/Bios-Marcel/discordemojimap"
	"github.com/agnivade/levenshtein"
	petname "github.com/dustinkirkland/golang-petname"
)

var (
	createDeleteMutex          = &sync.Mutex{}
	lobbies           []*Lobby = nil
)

var (
	LobbySettingBounds = &SettingBounds{
		MinDrawingTime:       60,
		MaxDrawingTime:       300,
		MinRounds:            1,
		MaxRounds:            20,
		MinMaxPlayers:        2,
		MaxMaxPlayers:        24,
		MinClientsPerIPLimit: 1,
		MaxClientsPerIPLimit: 24,
	}
	SupportedLanguages = map[string]string{
		"english": "English",
		"italian": "Italian",
		"german":  "German",
	}
)

// SettingBounds defines the lower and upper bounds for the user-specified
// lobby creation input.
type SettingBounds struct {
	MinDrawingTime       int64
	MaxDrawingTime       int64
	MinRounds            int64
	MaxRounds            int64
	MinMaxPlayers        int64
	MaxMaxPlayers        int64
	MinClientsPerIPLimit int64
	MaxClientsPerIPLimit int64
}

// LineEvent is basically the same as JSEvent, but with a specific Data type.
// We use this for reparsing as soon as we know that the type is right. It's
// a bit unperformant, but will do for now.
type LineEvent struct {
	Type string `json:"type"`
	Data *Line  `json:"data"`
}

// LineEvent is basically the same as JSEvent, but with a specific Data type.
// We use this for reparsing as soon as we know that the type is right. It's
// a bit unperformant, but will do for now.
type FillEvent struct {
	Type string `json:"type"`
	Data *Fill  `json:"data"`
}

func HandleEvent(raw []byte, received *JSEvent, lobby *Lobby, player *Player) error {
	if received.Type == "message" {
		dataAsString, isString := (received.Data).(string)
		if !isString {
			return fmt.Errorf("invalid data received: '%s'", received.Data)
		}

		if strings.HasPrefix(dataAsString, "!") {
			handleCommand(dataAsString[1:], player, lobby)
		} else {
			handleMessage(dataAsString, player, lobby)
		}
	} else if received.Type == "line" {
		if lobby.Drawer == player {
			line := &LineEvent{}
			jsonError := json.Unmarshal(raw, line)
			if jsonError != nil {
				return fmt.Errorf("error decoding data: %s", jsonError)
			}
			lobby.AppendLine(line)

			//We directly forward the event, as it seems to be valid.
			SendDataToConnectedPlayers(player, lobby, received)
		}
	} else if received.Type == "fill" {
		if lobby.Drawer == player {
			fill := &FillEvent{}
			jsonError := json.Unmarshal(raw, fill)
			if jsonError != nil {
				return fmt.Errorf("error decoding data: %s", jsonError)
			}
			lobby.AppendFill(fill)

			//We directly forward the event, as it seems to be valid.
			SendDataToConnectedPlayers(player, lobby, received)
		}
	} else if received.Type == "clear-drawing-board" {
		if lobby.Drawer == player {
			lobby.ClearDrawing()
			SendDataToConnectedPlayers(player, lobby, received)
		}
	} else if received.Type == "choose-word" {
		chosenIndex, isInt := (received.Data).(int)
		if !isInt {
			asFloat, isFloat32 := (received.Data).(float64)
			if isFloat32 && asFloat < 4 {
				chosenIndex = int(asFloat)
			} else {
				fmt.Println("Invalid data")
				return nil
			}
		}

		drawer := lobby.Drawer
		if player == drawer && len(lobby.WordChoice) > 0 && chosenIndex >= 0 && chosenIndex <= 2 {
			lobby.CurrentWord = lobby.WordChoice[chosenIndex]
			lobby.WordChoice = nil
			lobby.WordHints = createWordHintFor(lobby.CurrentWord, false)
			lobby.WordHintsShown = createWordHintFor(lobby.CurrentWord, true)
			triggerWordHintUpdate(lobby)
			WriteAsJSON(lobby.Drawer, JSEvent{Type: "your-turn"})
		}
	} else if received.Type == "kick-vote" {
		toKickID, isString := (received.Data).(string)
		if !isString {
			fmt.Println("Invalid data")
			return nil
		}
		if !lobby.EnableVotekick {
			// Votekicking is disabled in the lobby
			// We tell the user and do not continue with the event
			WriteAsJSON(player, JSEvent{Type: "system-message", Data: "Votekick is disabled in this lobby!"})
		} else {
			handleKickEvent(lobby, player, toKickID)
		}
	}

	return nil
}

func handleMessage(input string, sender *Player, lobby *Lobby) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return
	}

	if lobby.CurrentWord == "" {
		sendMessageToAll(trimmed, sender, lobby)
		return
	}

	if sender.State == Drawing || sender.State == Standby {
		sendMessageToAllNonGuessing(trimmed, sender, lobby)
	} else if sender.State == Guessing {
		lowerCasedInput := strings.ToLower(trimmed)
		lowerCasedSearched := strings.ToLower(lobby.CurrentWord)
		if lowerCasedSearched == lowerCasedInput {
			secondsLeft := lobby.RoundEndTime/1000 - time.Now().UTC().UnixNano()/1000000000
			sender.LastScore = int(math.Ceil(math.Pow(math.Max(float64(secondsLeft), 1), 1.3) * 2))
			sender.Score += sender.LastScore
			lobby.scoreEarnedByGuessers += sender.LastScore
			sender.State = Standby
			WriteAsJSON(sender, JSEvent{Type: "system-message", Data: "You have correctly guessed the word."})

			if !lobby.isAnyoneStillGuessing() {
				endRound(lobby)
			} else {
				//Since the word has been guessed correctly, we reveal it.
				WriteAsJSON(sender, JSEvent{Type: "update-wordhint", Data: lobby.WordHintsShown})
				recalculateRanks(lobby)
				triggerCorrectGuessEvent(lobby)
				triggerPlayersUpdate(lobby)
			}

			return
		} else if levenshtein.ComputeDistance(lowerCasedInput, lowerCasedSearched) == 1 {
			WriteAsJSON(sender, JSEvent{Type: "system-message", Data: fmt.Sprintf("'%s' is very close.", trimmed)})
		}

		sendMessageToAll(trimmed, sender, lobby)
	}
}

func (lobby *Lobby) isAnyoneStillGuessing() bool {
	for _, otherPlayer := range lobby.Players {
		if otherPlayer.State == Guessing {
			return true
		}
	}

	return false
}

func sendMessageToAll(message string, sender *Player, lobby *Lobby) {
	escaped := html.EscapeString(discordemojimap.Replace(message))
	for _, target := range lobby.Players {
		WriteAsJSON(target, JSEvent{Type: "message", Data: Message{
			Author:  html.EscapeString(sender.Name),
			Content: escaped,
		}})
	}
}

func sendMessageToAllNonGuessing(message string, sender *Player, lobby *Lobby) {
	escaped := html.EscapeString(discordemojimap.Replace(message))
	for _, target := range lobby.Players {
		if target.State != Guessing {
			WriteAsJSON(target, JSEvent{Type: "non-guessing-player-message", Data: Message{
				Author:  html.EscapeString(sender.Name),
				Content: escaped,
			}})
		}
	}
}

func handleKickEvent(lobby *Lobby, player *Player, toKickID string) {
	//Kicking yourself isn't allowed
	if toKickID == player.ID {
		return
	}

	//A player can't vote twice to kick someone
	if player.votedForKick[toKickID] {
		return
	}

	toKick := -1
	for index, otherPlayer := range lobby.Players {
		if otherPlayer.ID == toKickID {
			toKick = index
			break
		}
	}

	//If we haven't found the player, we can't kick him/her.
	if toKick != -1 {
		player.votedForKick[toKickID] = true
		playerToKick := lobby.Players[toKick]

		var voteKickCount int
		for _, otherPlayer := range lobby.Players {
			if otherPlayer.votedForKick[toKickID] == true {
				voteKickCount++
			}
		}

		votesNeeded := 1
		if len(lobby.Players)%2 == 0 {
			votesNeeded = len(lobby.Players) / 2
		} else {
			votesNeeded = (len(lobby.Players) / 2) + 1
		}

		WritePublicSystemMessage(lobby, fmt.Sprintf("(%d/%d) players voted to kick %s", voteKickCount, votesNeeded, playerToKick.Name))

		if voteKickCount >= votesNeeded {
			//Since the player is already kicked, we first clean up the kicking information related to that player
			for _, otherPlayer := range lobby.Players {
				if otherPlayer.votedForKick[toKickID] == true {
					delete(player.votedForKick, toKickID)
					break
				}
			}

			WritePublicSystemMessage(lobby, fmt.Sprintf("%s has been kicked from the lobby", playerToKick.Name))

			if lobby.Drawer == playerToKick {
				WritePublicSystemMessage(lobby, "Since the kicked player has been drawing, none of you will get any points this round.")
				//Since the drawing person has been kicked, that probably means that he/she was trolling, therefore
				//we redact everyones last earned score.
				for _, otherPlayer := range lobby.Players {
					otherPlayer.Score -= otherPlayer.LastScore
					otherPlayer.LastScore = 0
				}
				lobby.scoreEarnedByGuessers = 0
				//We must absolutely not set lobby.Drawer to nil, since this would cause the drawing order to be ruined.
			}

			if playerToKick.ws != nil {
				playerToKick.ws.Close()
			}
			lobby.Players = append(lobby.Players[:toKick], lobby.Players[toKick+1:]...)

			recalculateRanks(lobby)

			//If the owner is kicked, we choose the next best person as the owner.
			if lobby.Owner == playerToKick {
				for _, otherPlayer := range lobby.Players {
					potentialOwner := otherPlayer
					if potentialOwner.Connected {
						lobby.Owner = potentialOwner
						WritePublicSystemMessage(lobby, fmt.Sprintf("%s is the new lobby owner.", potentialOwner.Name))
						break
					}
				}
			}

			triggerPlayersUpdate(lobby)

			if lobby.Drawer == playerToKick || !lobby.isAnyoneStillGuessing() {
				endRound(lobby)
			}
		}
	}
}

func handleCommand(commandString string, caller *Player, lobby *Lobby) {
	command := commands.ParseCommand(commandString)
	if len(command) >= 1 {
		switch strings.ToLower(command[0]) {
		case "start":
			commandStart(caller, lobby)
		case "setmp":
			commandSetMP(caller, lobby, command)
		case "help":
			//TODO
		case "nick", "name", "username", "nickname", "playername", "alias":
			commandNick(caller, lobby, command)
		}
	}
}

func commandStart(caller *Player, lobby *Lobby) {
	if lobby.Round == 0 && caller == lobby.Owner {
		for _, otherPlayer := range lobby.Players {
			otherPlayer.Score = 0
			otherPlayer.LastScore = 0
		}

		advanceLobby(lobby)
	}
}

func commandNick(caller *Player, lobby *Lobby, args []string) {
	if len(args) == 1 {
		caller.Name = GeneratePlayerName()
		WriteAsJSON(caller, JSEvent{Type: "reset-username"})
		triggerPlayersUpdate(lobby)
	} else {
		//We join all arguments, since people won't sue quotes either way.
		//The input is trimmed and sanitized.
		newName := html.EscapeString(strings.TrimSpace(strings.Join(args[1:], " ")))
		if len(newName) == 0 {
			caller.Name = GeneratePlayerName()
			WriteAsJSON(caller, JSEvent{Type: "reset-username"})
		} else {
			fmt.Printf("%s is now %s\n", caller.Name, newName)
			//We don't want super-long names
			if len(newName) > 30 {
				newName = newName[:31]
			}
			caller.Name = newName
			WriteAsJSON(caller, JSEvent{Type: "persist-username", Data: newName})
		}
		triggerPlayersUpdate(lobby)
	}
}

func commandSetMP(caller *Player, lobby *Lobby, args []string) {
	if caller == lobby.Owner {
		if len(args) < 2 {
			return
		}

		newMaxPlayersValue := strings.TrimSpace(args[1])
		newMaxPlayersValueInt, err := strconv.ParseInt(newMaxPlayersValue, 10, 64)
		if err == nil {
			if int(newMaxPlayersValueInt) >= len(lobby.Players) && newMaxPlayersValueInt <= LobbySettingBounds.MaxMaxPlayers && newMaxPlayersValueInt >= LobbySettingBounds.MinMaxPlayers {
				lobby.MaxPlayers = int(newMaxPlayersValueInt)

				WritePublicSystemMessage(lobby, fmt.Sprintf("MaxPlayers value has been changed to %d", lobby.MaxPlayers))
			} else {
				if len(lobby.Players) > int(LobbySettingBounds.MinMaxPlayers) {
					WriteAsJSON(caller, JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value should be between %d and %d.", len(lobby.Players), LobbySettingBounds.MaxMaxPlayers)})
				} else {
					WriteAsJSON(caller, JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value should be between %d and %d.", LobbySettingBounds.MinMaxPlayers, LobbySettingBounds.MaxMaxPlayers)})
				}
			}
		} else {
			WriteAsJSON(caller, JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value must be numeric.")})
		}
	} else {
		WriteAsJSON(caller, JSEvent{Type: "system-message", Data: fmt.Sprintf("Only the lobby owner can change MaxPlayers setting.")})
	}
}

func endRound(lobby *Lobby) {
	var roundOverMessage string
	if lobby.CurrentWord == "" {
		roundOverMessage = "Round over. No word was chosen."
	} else {
		roundOverMessage = fmt.Sprintf("Round over. The word was '%s'", lobby.CurrentWord)
	}

	//The drawer can potentially be null if he's kicked, in that case we proceed with the round if anyone has already
	drawer := lobby.Drawer
	if drawer != nil && lobby.scoreEarnedByGuessers > 0 {
		averageScore := float64(lobby.scoreEarnedByGuessers) / float64(len(lobby.Players)-1)
		if averageScore > 0 {
			drawer.LastScore = int(averageScore * 1.1)
			drawer.Score += drawer.LastScore
		}
	}

	lobby.scoreEarnedByGuessers = 0
	lobby.alreadyUsedWords = append(lobby.alreadyUsedWords, lobby.CurrentWord)
	lobby.CurrentWord = ""
	lobby.WordHints = nil

	//If the round ends and people still have guessing, that means the "Last" value
	////for the next turn has to be "no score earned".
	for _, otherPlayer := range lobby.Players {
		if otherPlayer.State == Guessing {
			otherPlayer.LastScore = 0
		}
	}

	WritePublicSystemMessage(lobby, roundOverMessage)

	advanceLobby(lobby)
}

func advanceLobby(lobby *Lobby) {
	if lobby.timeLeftTicker != nil {
		lobby.timeLeftTicker.Stop()
		lobby.timeLeftTicker = nil
		lobby.timeLeftTickerReset <- struct{}{}
	}

	for _, otherPlayer := range lobby.Players {
		otherPlayer.State = Guessing
		otherPlayer.votedForKick = make(map[string]bool)
	}

	lobby.ClearDrawing()

	if lobby.Drawer == nil {
		lobby.Drawer = lobby.Players[0]
		lobby.Round++
	} else {
		//If everyone has drawn once (e.g. a round has passed)
		if lobby.Drawer == lobby.Players[len(lobby.Players)-1] {
			if lobby.Round == lobby.MaxRounds {
				lobby.Drawer = nil
				lobby.Round = 0

				recalculateRanks(lobby)
				triggerPlayersUpdate(lobby)

				WritePublicSystemMessage(lobby, "Game over. Type !start again to start a new round.")

				return
			}

			lobby.Round++
			lobby.Drawer = lobby.Players[0]
		} else {
			selectNextDrawer(lobby)
		}
	}

	lobby.Drawer.State = Drawing
	lobby.WordChoice = GetRandomWords(lobby)
	WriteAsJSON(lobby.Drawer, JSEvent{Type: "prompt-words", Data: lobby.WordChoice})

	//We use milliseconds for higher accuracy
	lobby.RoundEndTime = time.Now().UTC().UnixNano()/1000000 + int64(lobby.DrawingTime)*1000
	lobby.timeLeftTicker = time.NewTicker(1 * time.Second)
	go func() {
		showNextHintInSeconds := lobby.DrawingTime / 3
		hintsLeft := 2

		for {
			select {
			case <-lobby.timeLeftTicker.C:
				if hintsLeft > 0 {
					showNextHintInSeconds--
					if showNextHintInSeconds == 0 {
						showNextHintInSeconds = lobby.DrawingTime / 3
						hintsLeft--
						//FIXME If a word is chosen too late, less hints will come overall.
						if lobby.WordHints != nil {
							for {
								randomIndex := rand.Int() % len(lobby.WordHints)
								if lobby.WordHints[randomIndex].Character == 0 {
									lobby.WordHints[randomIndex].Character = []rune(lobby.CurrentWord)[randomIndex]
									triggerWordHintUpdate(lobby)
									break
								}
							}
						}
					}
				}
				if lobby.RoundEndTime == 0 {
					go endRound(lobby)
				}
			case <-lobby.timeLeftTickerReset:
				return
			}
		}
	}()

	recalculateRanks(lobby)

	TriggerComplexUpdateEvent("next-turn", &NextTurn{
		Round:        lobby.Round,
		Players:      lobby.Players,
		RoundEndTime: lobby.RoundEndTime,
	}, lobby)
}

// NextTurn represents the data necessary for displaying the lobby state right
// after a new turn started. Meaning that no word has been chosen yet and
// therefore there are no wordhints and no current drawing instructions.
type NextTurn struct {
	Round        int       `json:"round"`
	Players      []*Player `json:"players"`
	RoundEndTime int64     `json:"roundEndTime"`
}

func recalculateRanks(lobby *Lobby) {
	for _, a := range lobby.Players {
		playersThatAreHigher := 0
		for _, b := range lobby.Players {
			if b.Score > a.Score {
				playersThatAreHigher++
			}
		}

		a.Rank = playersThatAreHigher + 1
	}
}

func selectNextDrawer(lobby *Lobby) {
	for playerIndex, otherPlayer := range lobby.Players {
		if otherPlayer == lobby.Drawer {
			lobby.Drawer = lobby.Players[playerIndex+1]
			return
		}
	}

	for _, otherPlayer := range lobby.Players {
		if otherPlayer == lobby.Drawer {
			return
		}

		if !otherPlayer.Connected {
			continue
		}

		lobby.Drawer = otherPlayer
	}
}

func createWordHintFor(word string, showAll bool) []*WordHint {
	wordHints := make([]*WordHint, 0, len(word))
	for _, char := range word {
		irrelevantChar := char == ' ' || char == '_' || char == '-'
		if showAll {
			wordHints = append(wordHints, &WordHint{
				Character: char,
				Underline: !irrelevantChar,
			})
		} else {
			wordHints = append(wordHints, &WordHint{
				Underline: !irrelevantChar,
			})
		}
	}

	return wordHints
}

var TriggerSimpleUpdateEvent func(eventType string, lobby *Lobby)
var TriggerComplexUpdatePerPlayerEvent func(eventType string, data func(*Player) interface{}, lobby *Lobby)
var TriggerComplexUpdateEvent func(eventType string, data interface{}, lobby *Lobby)
var SendDataToConnectedPlayers func(sender *Player, lobby *Lobby, data interface{})
var WriteAsJSON func(player *Player, object interface{}) error
var WritePublicSystemMessage func(lobby *Lobby, text string)

func triggerPlayersUpdate(lobby *Lobby) {
	TriggerComplexUpdateEvent("update-players", lobby.Players, lobby)
}

func triggerCorrectGuessEvent(lobby *Lobby) {
	TriggerSimpleUpdateEvent("correct-guess", lobby)
}

func triggerWordHintUpdate(lobby *Lobby) {
	if lobby.CurrentWord == "" {
		return
	}

	TriggerComplexUpdatePerPlayerEvent("update-wordhint", func(player *Player) interface{} {
		return lobby.GetAvailableWordHints(player)
	}, lobby)
}

type Rounds struct {
	Round     int `json:"round"`
	MaxRounds int `json:"maxRounds"`
}

// CreateLobby allows creating a lobby, optionally returning errors that
// occured during creation.
func CreateLobby(playerName, language string, drawingTime, rounds, maxPlayers, customWordChance, clientsPerIPLimit int, customWords []string, enableVotekick bool) (string, *Lobby, error) {
	lobby := createLobby(drawingTime, rounds, maxPlayers, customWords, customWordChance, clientsPerIPLimit, enableVotekick)
	player := createPlayer(playerName)

	lobby.Players = append(lobby.Players, player)
	lobby.Owner = player

	// Read wordlist according to the chosen language
	words, err := readWordList(language)
	if err != nil {
		//TODO Remove lobby, since we errored.
		return "", nil, err
	}

	lobby.Words = words

	return player.userSession, lobby, nil
}

// GeneratePlayerName creates a new playername. A so called petname. It consists
// of an adverb, an adjective and a animal name. The result can generally be
// trusted to be sane.
func GeneratePlayerName() string {
	adjective := strings.Title(petname.Adjective())
	adverb := strings.Title(petname.Adverb())
	name := strings.Title(petname.Name())
	return adverb + adjective + name
}

// Message represents a message in the chatroom.
type Message struct {
	// Author is the player / thing that wrote the message
	Author string
	// Content is the actual message text.
	Content string
}

// Ready represents the initial state that a user needs upon connection.
// This includes all the necessary things for properly running a client
// without receiving any more data.
type Ready struct {
	PlayerID string `json:"playerId"`
	Drawing  bool   `json:"drawing"`

	Round          int           `json:"round"`
	MaxRound       int           `json:"maxRounds"`
	RoundEndTime   int64         `json:"roundEndTime"`
	WordHints      []*WordHint   `json:"wordHints"`
	Players        []*Player     `json:"players"`
	CurrentDrawing []interface{} `json:"currentDrawing"`
}

func OnConnected(lobby *Lobby, player *Player) {
	player.Connected = true
	WriteAsJSON(player, JSEvent{Type: "ready", Data: &Ready{
		PlayerID: player.ID,
		Drawing:  player.State == Drawing,

		Round:          lobby.Round,
		MaxRound:       lobby.MaxRounds,
		RoundEndTime:   lobby.RoundEndTime,
		WordHints:      lobby.GetAvailableWordHints(player),
		Players:        lobby.Players,
		CurrentDrawing: lobby.CurrentDrawing,
	}})

	//TODO Only send to everyone except for the new player, since it's part of the ready event.
	triggerPlayersUpdate(lobby)
}

func OnDisconnected(lobby *Lobby, player *Player) {
	//We want to avoid calling the handler twice.
	if player.ws == nil {
		return
	}

	player.Connected = false
	player.ws = nil

	if !lobby.HasConnectedPlayers() {
		RemoveLobby(lobby.ID)
		log.Printf("There are currently %d open lobbies.\n", len(lobbies))
	} else {
		triggerPlayersUpdate(lobby)
	}
}

func (lobby *Lobby) GetAvailableWordHints(player *Player) []*WordHint {
	//The draw simple gets every character as a word-hint. We basically abuse
	//the hints for displaying the word, instead of having yet another GUI
	//element that wastes space.
	if player.State == Drawing || player.State == Standby {
		return lobby.WordHintsShown
	} else {
		return lobby.WordHints
	}
}

func (lobby *Lobby) JoinPlayer(playerName string) string {
	player := createPlayer(playerName)

	//FIXME Make a dedicated method that uses a mutex?
	lobby.Players = append(lobby.Players, player)
	recalculateRanks(lobby)
	triggerPlayersUpdate(lobby)

	return player.userSession
}