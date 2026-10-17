package hostkit

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// LoginFields returns the five SIP login fields (IDs sipaccount.Field*, in this order: username,
// password, registrar, transport, display number). Transport is optional with default "udp"; the phone
// number is normalized with defaultCC.
func LoginFields(defaultCC string) []bridgev2.LoginInputDataField {
	return []bridgev2.LoginInputDataField{
		{
			Type:        bridgev2.LoginInputFieldTypeUsername,
			ID:          sipaccount.FieldUsername,
			Name:        "SIP username",
			Description: "The SIP account username. For JMP this is the numeric SIP username, not your phone number.",
		},
		{
			Type:        bridgev2.LoginInputFieldTypePassword,
			ID:          sipaccount.FieldPassword,
			Name:        "SIP password",
			Description: "Your SIP account password.",
		},
		{
			Type:        bridgev2.LoginInputFieldTypeDomain,
			ID:          sipaccount.FieldRegistrar,
			Name:        "Registrar",
			Description: "Registrar host:port from your provider or ATA config, e.g. example.auth.bandwidth.com:5008. Port defaults to 5060.",
			Validate:    sipaccount.NormalizeRegistrar,
		},
		{
			Type:         bridgev2.LoginInputFieldTypeSelect,
			ID:           sipaccount.FieldTransport,
			Name:         "Transport",
			Description:  "SIP transport. Use udp unless your provider says otherwise.",
			Options:      []string{"udp", "tcp"},
			DefaultValue: "udp",
			Optional:     true,
			Validate: func(s string) (string, error) {
				t, err := sipua.ParseTransport(s)
				return string(t), err
			},
		},
		{
			Type:        bridgev2.LoginInputFieldTypePhoneNumber,
			ID:          sipaccount.FieldDisplayNumber,
			Name:        "Phone number",
			Description: "The account's phone number, e.g. +15551234567 (numbers without + use the default country code).",
			Validate: func(s string) (string, error) {
				return phone.Normalize(s, defaultCC)
			},
		},
	}
}

// PromptFields runs bridgev2's one-field-per-message prompt flow for fields (same texts as
// bridgev2/commands/login.go userInputLoginCommandState: "Please enter your <Name>", description,
// options, "Enter `skip` to skip this field"), storing commands.CommandState{Action: action, Cancel:
// onCancel}. Password and token fields are redacted with ce.Redact() before validation. Invalid input
// re-prompts the same field. When all fields are filled it clears the state and calls done(ce, input).
func PromptFields(ce *commands.Event, action string, fields []bridgev2.LoginInputDataField,
	done func(ce *commands.Event, input map[string]string), onCancel func()) {
	ps := &promptState{
		action:    action,
		remaining: append([]bridgev2.LoginInputDataField(nil), fields...),
		data:      make(map[string]string, len(fields)),
		done:      done,
		onCancel:  onCancel,
	}
	if len(ps.remaining) == 0 {
		commands.StoreCommandState(ce.User, nil)
		done(ce, ps.data)
		return
	}
	ps.promptNext(ce)
}

// promptState is the command state of one PromptFields run.
type promptState struct {
	action    string
	remaining []bridgev2.LoginInputDataField
	data      map[string]string
	done      func(ce *commands.Event, input map[string]string)
	onCancel  func()
}

func (ps *promptState) promptNext(ce *commands.Event) {
	field := ps.remaining[0]
	parts := []string{fmt.Sprintf("Please enter your %s", field.Name)}
	if field.Description != "" {
		parts = append(parts, field.Description)
	}
	if len(field.Options) > 0 {
		parts = append(parts, fmt.Sprintf("Options: `%s`", strings.Join(field.Options, "`, `")))
	}
	if field.Optional {
		parts = append(parts, "Enter `skip` to skip this field")
	}
	// Like bridgev2's prompt, $cmdprefix in field texts is expanded (Reply only expands its format).
	ce.Reply("%s", strings.ReplaceAll(strings.Join(parts, "\n"), "$cmdprefix", ce.Bridge.Config.CommandPrefix))
	commands.StoreCommandState(ce.User, &commands.CommandState{
		Next:   commands.MinimalCommandHandlerFunc(ps.submitNext),
		Action: ps.action,
		Meta:   ps,
		Cancel: ps.onCancel,
	})
}

func (ps *promptState) submitNext(ce *commands.Event) {
	field := ps.remaining[0]
	field.FillDefaultValidate()
	if field.Type == bridgev2.LoginInputFieldTypePassword || field.Type == bridgev2.LoginInputFieldTypeToken {
		ce.Redact()
	}
	val := ce.RawArgs
	if field.Optional && ce.RawArgs == "skip" {
		val = ""
	}
	v, err := field.Validate(val)
	if err != nil {
		ce.Reply("Invalid value: %v", err)
		return
	}
	ps.data[field.ID] = v
	if len(ps.remaining) > 1 {
		ps.remaining = ps.remaining[1:]
		ps.promptNext(ce)
		return
	}
	commands.StoreCommandState(ce.User, nil)
	ps.done(ce, ps.data)
}
