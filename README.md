# sasayaki

unofficial implementation of the bluesky dm service, for the bit

useful to AppViews that are based off the main bluesky appview.

keep in mind that at the moment (january 2025), bsky chat is a centralized service living on https://api.bsky.chat.
in turn, to make this work within the atproto mainnet, you and your friends need to use the same build of social-app
that uses your own dm service. more information in the **how** section

![screenshot of it working](https://smooch.computer/i/9sem1xrewiu5k.png)

## how

- get go1.23 because indigo depends on it: https://go.dev/dl/

```
git clone https://github.com/lun-4/sasayaki
cd sasayaki
env APPVIEW_URL=https://appview.example.net \
	ATPROTO_PLC_URL=https://plc.example.net \
	SERVER_URL=chat.example.net \
	DB_PATH=data.db \
	PORT=43091 go run .
```

then set chat.example.net as the XRPC service inside social-app, this requires social-app patches.

`EXPO_PUBLIC_BSKY_CHAT_DID_REFERENCE=did:web:chat.example.net#bsky_chat`

more info on: https://l4.pm/wiki/Personal%20Wiki/bluesky/bsky%20independent%20appview/a%20step%20by%20step%20process%20into%20an%20appview.html#social-app
