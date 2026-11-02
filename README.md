![Project Logo](assets/logo.svg)

Reverse engineered AirDrop for Windows and Linux

Bare with me, this is a slight mess of a README and I will clean it up and polish it when I get a chance. 

> [!WARNING]
> This is a VERY early proof of concept/alpha. I've only been working on it for several weeks, so don't expect it to be amazing right out the gate. On top of that, I do want to make it very clear that I did use an LLM to help write, clean up, and structure the code along my base framework.
> This is **not** even close to being finished.

### So... Why?
So why on earth am I doing this when Localsend and Quickshare and a multitude of other solutions are out there? Simply put, in my opinion they are not as convenient as AirDrop. Especially if a non-techy friend just wants to quickly AirDrop me something. 

Android recently got AirDrop support via Quickshare as a result of Apple adopting WiFi Aware (NAN) along with AWDL on newer versions of iOS/macOS.

In past, the kexts `itlwm` and `AirportItlwm` couldn't support AirDrop due to AWDL using a custom 802.11 channel to make a virtual awdl0 IPv6 local connection. 

### How it works in a nutshell
1. AirDrop is a 22b bluetooth signal essentially (ID 0x004C and Type 0x05 ) with a 2 byte SHA-256 hash which means that a bluetooth packet can be broadcasted out so if you have your iOS/macOS device set to 'Everyone' it can pick it up.

2. AWDL (Apple Wireless Direct Link) is horrible and I hate it. It uses a custom 802.11 channel which means it's really complicated to get a virtual AWDL signal using IPv6. That's actually why the kexts itlwm and AirportItlwm didn't have AirDrop. What HackDrop does, (to be clear, it works, it's just very buggy. I've only worked on this for a week.) is it builds a virtual AWDL and a NAN that supports IPv6 locally.

3. Once you have a IPv6 then it actually gets incredibly simple. All you need is Bonjour/mDNS spinning up a TLS HTTPS server and exchanging the property lists: /Discover /Ask /Upload

### What's Working
* AirDrop HTTPS Server/Client (port 8770 with a self signed X.509 TLS cert) 
* POSIX Gzip CPIO Packer/Extraction (packs/unpacks files and handles decoding)
* Binary Plist (A Go encoder and parser)
* mDNS Scanner/Responder (IPv4|6 works locally with IP probing)
* Localsend-ish GUI (Wails GUI that I had Gemini build for now, and plan to make myself once I finish learning how)
* Actually connected to a MacBook Pro 2019 (26), Neo (27), and an iMac 2015 (12).

### Not Working / WIP
* Contacts Only AirDrop - Probably will not be supported and I may not work on it. It needs Apple signed iCloud certs.
* Keeping a connection to macOS Tahoe or Golden Gate before it crashes after two minutes. Newer `sharingd` versions drop P2P signals over AWDL incredibly quickly since the frames aren't synced.
* Full Kernal Drivers - (`nl80211 and IO80211` kext) Need it to hook up a raw `nl80211` monitor frame injection for Linux. And also to mess around with a kext/daemon for Sonoma/Sequoia/Tahoe on Intel WiFi chips (`itlwm`)

## Getting Started
Two ways you can go with this, either download the pre-built binaries, or build it from source.
A. Grab your binary from releases
  * Windows (amd64): hackdrop.exe
  * Linux (amd64): hackdrop-linux-amd64
  * macOS Apple Silicon (arm64): hackdrop-darwin-arm64
  * macOS Intel / Hackintosh (amd64): hackdrop-darwin-amd64
B. If you are going to build it from source, it needs Go 1.22+. If you are going that route, I'm going to assume you already know how to compile something in Go or you are going to have your LLM do it for you. 

### Quick Note
By default, macOS `sharingd` ignores `_airdrop._tcp.local.` entirely. mDNS won't work unless it's over a native Broadcom `awdl0` interface. To solve this slight issue allowing Finder/sharingd to discover AirDrop peers, run these two commands in terminal:

```defaults write com.apple.NetworkBrowser BrowseAllInterfaces -bool true```

```killall Finder sharingd```

### Contributing
If you know anything about:

- **macOS** `sharingd`, `NetworkBrowser`, `IO80211Family`, `itlwm`

- **Linux** `nl80211` monitoring, active monitor, or kernel NAN

- **Go and Wails v2**

Then feel free to open an issue or a PR!

#### License & Disclaimer (yay legal things)
Licensed under the MIT License.
HackDrop is an independent open-source project and is not affiliated with, endorsed by, or sponsored by Apple Inc. AirDrop, macOS, iOS, and Bonjour are trademarks of Apple Inc.
