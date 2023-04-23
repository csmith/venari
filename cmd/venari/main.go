package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/csmith/envflag"
)

var (
	discordToken     = flag.String("discord-token", "", "Bot token to use to connect to discord")
	discordTestGuild = flag.String("discord-test-guild", "", "Guild ID to register commands in for testing purposes")
	activeCategory   = flag.String("discord-active-category", "Active hunts", "Discord category for active hunts")
	archiveCategory  = flag.String("discord-archive-category", "Archived hunts", "Discord category for archived hunts")
)

var commands = []*discordgo.ApplicationCommand{
	{
		Name:        "hunt",
		Description: "Create a new hunt with the given name",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "name",
				Description: "Name of the puzzle hunt",
				Required:    true,
			},
		},
	},
	{
		Name:        "archive",
		Description: "Archives a hunt",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionChannel,
				Name:        "channel",
				Description: "Channel to be archived",
				Required:    true,
			},
		},
	},
}

func main() {
	envflag.Parse(envflag.WithPrefix("VENARI_"))

	d, err := discordgo.New(fmt.Sprintf("Bot %s", *discordToken))
	if err != nil {
		log.Fatalf("Failed to create discord session: %v", err)
	}

	d.StateEnabled = true
	d.State.TrackChannels = true
	d.State.TrackRoles = true

	if err := d.Open(); err != nil {
		log.Fatalf("Failed to open discord session: %v", err)
		return
	}

	updateCommands(d, *discordTestGuild)
	d.AddHandler(handleInteraction)

	log.Print("Up and running!")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	log.Print("Shutting down...")
	_ = d.Close()
}

func updateCommands(s *discordgo.Session, guild string) {
	existing, err := s.ApplicationCommands(s.State.User.ID, guild)
	if err != nil {
		log.Fatalf("Unable to list commands: %v", err)
	}

	// TODO: Delete commands no longer present
	for i := range commands {
		c := commands[i]
		update := true
		for j := range existing {
			e := existing[j]
			if e.Name == c.Name {
				update = !reflect.DeepEqual(e.Description, c.Description) || !reflect.DeepEqual(e.Options, c.Options)
			}
		}

		if update {
			log.Printf("Updating command %s for guild '%s'", c.Name, guild)
			_, err := s.ApplicationCommandCreate(s.State.User.ID, guild, c)
			if err != nil {
				log.Fatalf("Cannot create '%v' command: %v", c.Name, err)
			}
		}
	}
}

func handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	payload := i.Interaction.Data.(discordgo.ApplicationCommandInteractionData)

	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("Failed to ACK command: %v", err)
	}

	go func() {
		respond := func(message string) {
			_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
				Content: message,
			})

			if err != nil {
				log.Fatalf("Unable to send command response: %v", err)
			}
		}

		switch payload.Name {
		case "hunt":
			createHunt(s, i.GuildID, respond, payload.Options[0].StringValue())
		case "archive":
			archiveHunt(s, i.GuildID, respond, payload.Options[0].ChannelValue(s))
		}
	}()
}

var disallowedCharsRegex = regexp.MustCompile("[^a-zA-Z0-9-]")

func createHunt(s *discordgo.Session, guildId string, respond func(message string), name string) {
	normalised := disallowedCharsRegex.ReplaceAllString(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "")
	roleName := fmt.Sprintf("hunt-%s", normalised)

	category := findCategory(s, guildId, *activeCategory)

	role, err := s.GuildRoleCreate(
		guildId,
		&discordgo.RoleParams{
			Name: roleName,
		},
	)
	if err != nil {
		log.Fatalf("Failed to create role %s in guild %s: %v", roleName, guildId, err)
	}

	_, err = s.GuildChannelCreateComplex(guildId, discordgo.GuildChannelCreateData{
		Type:     discordgo.ChannelTypeGuildText,
		Name:     normalised,
		ParentID: category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:    role.ID,
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionAllText,
			},
			{
				ID:   guildId,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionAll,
			},
		},
	})
	if err != nil {
		log.Fatalf("Failed to create text channel %s in guild %s: %v", normalised, guildId, err)
	}

	_, err = s.GuildChannelCreateComplex(guildId, discordgo.GuildChannelCreateData{
		Type:     discordgo.ChannelTypeGuildVoice,
		Name:     normalised,
		ParentID: category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:    role.ID,
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionAllVoice,
			},
			{
				ID:   guildId,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionAll,
			},
		},
	})
	if err != nil {
		log.Fatalf("Failed to create voice channel %s in guild %s: %v", normalised, guildId, err)
	}

	respond(fmt.Sprintf("Hunt created: %s", normalised))
}

func archiveHunt(s *discordgo.Session, guildId string, respond func(message string), target *discordgo.Channel) {
	log.Printf("Archive request for channel %s in guild %s", target.Name, guildId)

	active := findCategory(s, guildId, *activeCategory)
	archive := findCategory(s, guildId, *archiveCategory)

	if target.ParentID != active.ID {
		respond("That channel doesn't seem to be an active hunt channel. Do better.")
		return
	}

	channels, err := s.GuildChannels(guildId)
	if err != nil {
		log.Fatalf("Failed to retrieve channels for guild %s: %v", guildId, err)
	}

	for _, c := range channels {
		if c.Name == target.Name && c.ParentID == archive.ID {
			if c.Type == discordgo.ChannelTypeGuildVoice {
				log.Printf("Deleting voice channel %s", c.Name)
				_, err := s.ChannelDelete(c.ID)
				if err != nil {
					log.Fatalf("Failed to delete voice channel %s in guild %s: %v", target.Name, guildId, err)
				}
			} else if c.Type == discordgo.ChannelTypeGuildText {
				log.Printf("Deleting text channel %s", c.Name)
				_, err := s.ChannelEdit(c.ID, &discordgo.ChannelEdit{
					Name:                 fmt.Sprintf("%s-%s", time.Now().Format("2006-01"), c.Name),
					ParentID:             archive.ID,
					PermissionOverwrites: archive.PermissionOverwrites,
				})
				if err != nil {
					log.Fatalf("Failed to edit text channel %s in guild %s: %v", target.Name, guildId, err)
				}
			}
		}
	}

	roles, err := s.GuildRoles(guildId)
	if err != nil {
		log.Fatalf("Failed to retrieve roles for guild %s: %v", guildId, err)
	}

	for _, r := range roles {
		if r.Name == fmt.Sprintf("hunt-%s", target.Name) {
			log.Printf("Deleting role %s", r.Name)
			err := s.GuildRoleDelete(guildId, r.ID)
			if err != nil {
				log.Fatalf("Failed to delete role %s in guild %s: %v", r.Name, guildId, err)
			}
		}
	}

	respond(fmt.Sprintf("Hunt archived"))
}

func findCategory(s *discordgo.Session, guildId string, categoryName string) *discordgo.Channel {
	g, err := s.State.Guild(guildId)
	if err != nil {
		log.Fatalf("Failed to retrieve guild state for guild %s: %v", guildId, err)
	}

	for i := range g.Channels {
		if g.Channels[i].Type == discordgo.ChannelTypeGuildCategory && g.Channels[i].Name == categoryName {
			return g.Channels[i]
		}
	}

	c, err := s.GuildChannelCreateComplex(guildId, discordgo.GuildChannelCreateData{
		Type: discordgo.ChannelTypeGuildCategory,
		Name: categoryName,
	})
	if err != nil {
		log.Fatalf("Failed to create category %s in guild %s: %v", categoryName, guildId, err)
	}
	return c
}
