#!/usr/bin/env bash
# shellcheck disable=SC2034  # the texts are read through a name reference (t)
# Spectre Voice Server — install or update, step by step (Debian 12/13, Ubuntu 22.04+; LXC, VM or VPS).
#
#   curl -fsSL https://raw.githubusercontent.com/Stefan3Dx/SpectreVoice-Server/main/install.sh | sudo bash
#   sudo bash install.sh                 step by step: language, name and password, network, port, extras, firewall
#   sudo bash install.sh --yes           no questions: LAN server, defaults
#   sudo bash install.sh --update        newest version, settings kept
#   sudo bash install.sh --domain voice.example.com --email you@example.com --yes   public, HTTPS + TURN
#   sudo bash install.sh --source        build from this folder's source instead of downloading
#
# Options: --lang en|ro|es|de|pl|fr|it|pt|hu|tr  --name NAME  --password PASS  --domain NAME  --email ADDR  --contact ADDR
#          --public-ip IP|DDNS  --port N  --no-music  --no-turn  --no-firewall  --no-auto-update
#          --udp 50000-50200 (a range instead of one port)
set -euo pipefail

REPO="Stefan3Dx/SpectreVoice-Server"
GO_VERSION=1.26.0
BIN=/usr/local/bin/spectrevoice-server
ETC=/etc/spectrevoice
DATA=/var/lib/spectrevoice
ENVF=$ETC/server.env

YES=0 UPDATE=0 SOURCE=0 MUSIC=1 TURN=1 FIREWALL=1 AUTOUPD=1
NAME="" DOMAIN="" EMAIL="" CONTACT="" PUBLIC="" UDP="" PORT="" LANGSEL="" MODE="" SVPASS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes|-y) YES=1 ;;
    --update) UPDATE=1; YES=1 ;;
    --source) SOURCE=1 ;;
    --no-music) MUSIC=0 ;;
    --no-turn) TURN=0 ;;
    --no-firewall) FIREWALL=0 ;;
    --no-auto-update) AUTOUPD=0 ;;
    --lang) LANGSEL="$2"; shift ;;
    --name) NAME="$2"; shift ;;
    --password) SVPASS="$2"; shift ;;
    --domain) DOMAIN="$2"; shift ;;
    --email) EMAIL="$2"; shift ;;
    --contact) CONTACT="$2"; shift ;;
    --public-ip) PUBLIC="$2"; shift ;;
    --port) PORT="$2"; shift ;;
    --udp) UDP="$2"; shift ;;
    -h|--help) sed -n '3,15p' "$0"; exit 0 ;;
    *) echo "unknown option: $1"; exit 1 ;;
  esac
  shift
done

# ======================================================================================================================
# Texts: English is the reference; a missing text falls back to it.
# ======================================================================================================================
declare -A L_en=(
  [q_auto]="Automatic server updates (daily, around 4 AM)" [autodone]="Automatic updates: on"
  [title]="Spectre Voice Server — installer"
  [step]="Step %s of %s · %s"
  [s_lang]="Language" [s_name]="Name, password and contact" [s_net]="Network" [s_port]="Port" [s_extra]="Extras" [s_fw]="Firewall" [s_inst]="Installing"
  [q_name]="Server name"
  [q_contact]="Your e-mail for people's data requests (shown at /privacy)"
  [q_pass]="Server password (empty: anyone with the address gets in; admins can change it later in the app)"
  [net_q]="How will people reach this server?"
  [net_1]="My own domain, with HTTPS (e.g. voice.example.com)"
  [net_2]="A free automatic name, with HTTPS: %s"
  [net_3]="The public IP and a port, without HTTPS"
  [net_4]="Only my local network (LAN)"
  [choice]="Choice"
  [bad]="Choose one of the numbers above."
  [q_domain]="Domain (its DNS must already point to %s)"
  [q_email]="E-mail for the HTTPS certificate (Let's Encrypt)"
  [q_public]="Public IP (or DDNS name) the apps reach for voice"
  [q_lan]="This server's LAN address"
  [q_port]="Port for the apps and the voice (TCP and UDP)"
  [q_vport]="Port for the voice (UDP)"
  [q_turn]="TURN relay for networks that block UDP (coturn)"
  [q_music]="Music bot (!play: installs ffmpeg and yt-dlp)"
  [q_fw]="Open these ports in the firewall (%s)"
  [no_fw]="No active firewall here (ufw/firewalld). If your provider has one in its panel, open the ports there."
  [yn]="Y/n" [ny]="y/N"
  [pkgs]="Packages" [dl]="Downloading the server" [build]="No release to download: building from source"
  [music]="Music bot: ffmpeg and yt-dlp" [https]="HTTPS for %s (Caddy)" [turn]="TURN relay (coturn)" [fwdone]="Firewall: ports opened"
  [start]="Starting the server" [fail]="The server didn't start; see above."
  [ready]="Spectre Voice Server %s is running"
  [r_addr]="Server address" [r_port]="Port" [r_key]="Admin key"
  [r_how]="In the app: Server › Claim, then type the admin key. It makes you the server's owner (it works once)."
  [r_owned]="(the server already has an owner)"
  [r_pass]="Server password" [r_open]="none (open server)"
  [r_ports]="Ports to open" [r_set]="Settings" [r_logs]="Logs" [r_upd]="Update"
  [r_plain]="Without HTTPS the sign-in travels unencrypted (the voice itself stays encrypted). A domain is safer."
  [r_dns]="If HTTPS doesn't come up within a minute, check that the name points to this server and that TCP 80/443 are open."
  [nothing]="Nothing installed yet: installing with the defaults"
  [root]="Run as root: sudo bash install.sh"
  [enter]="Enter keeps the value in brackets."
)
declare -A L_ro=(
  [q_auto]="Actualizări automate ale serverului (zilnic, pe la 4 dimineața)" [autodone]="Actualizări automate: pornite"
  [title]="Spectre Voice Server — instalare"
  [step]="Pasul %s din %s · %s"
  [s_lang]="Limba" [s_name]="Nume, parolă și contact" [s_net]="Rețea" [s_port]="Port" [s_extra]="Extra" [s_fw]="Firewall" [s_inst]="Instalare"
  [q_name]="Numele serverului"
  [q_contact]="E-mailul tău pentru cererile oamenilor despre date (apare la /privacy)"
  [q_pass]="Parola serverului (gol: intră oricine are adresa; adminii o pot schimba apoi din aplicație)"
  [net_q]="Cum ajung oamenii la acest server?"
  [net_1]="Domeniul meu, cu HTTPS (ex. voice.exemplu.ro)"
  [net_2]="Un nume automat gratuit, cu HTTPS: %s"
  [net_3]="IP-ul public și un port, fără HTTPS"
  [net_4]="Doar rețeaua mea locală (LAN)"
  [choice]="Alegerea"
  [bad]="Alege unul dintre numerele de mai sus."
  [q_domain]="Domeniul (DNS-ul lui trebuie să arate deja spre %s)"
  [q_email]="E-mail pentru certificatul HTTPS (Let's Encrypt)"
  [q_public]="IP-ul public (sau numele DDNS) la care ajung aplicațiile pentru voce"
  [q_lan]="Adresa serverului în rețeaua locală"
  [q_port]="Portul pentru aplicații și voce (TCP și UDP)"
  [q_vport]="Portul pentru voce (UDP)"
  [q_turn]="Releu TURN pentru rețelele care blochează UDP (coturn)"
  [q_music]="Bot de muzică (!play: instalează ffmpeg și yt-dlp)"
  [q_fw]="Deschid aceste porturi în firewall (%s)"
  [no_fw]="Nu e niciun firewall activ aici (ufw/firewalld). Dacă furnizorul are unul în panou, deschide porturile acolo."
  [yn]="D/n" [ny]="d/N"
  [pkgs]="Pachete" [dl]="Descarc serverul" [build]="Nicio versiune de descărcat: compilez din sursă"
  [music]="Botul de muzică: ffmpeg și yt-dlp" [https]="HTTPS pentru %s (Caddy)" [turn]="Releu TURN (coturn)" [fwdone]="Firewall: porturi deschise"
  [start]="Pornesc serverul" [fail]="Serverul nu a pornit; vezi mai sus."
  [ready]="Spectre Voice Server %s rulează"
  [r_addr]="Adresa serverului" [r_port]="Portul" [r_key]="Cheia de admin"
  [r_how]="În aplicație: Server › Revendică, apoi scrie cheia de admin. Te face proprietarul serverului (merge o singură dată)."
  [r_owned]="(serverul are deja proprietar)"
  [r_pass]="Parola serverului" [r_open]="niciuna (server deschis)"
  [r_ports]="Porturi de deschis" [r_set]="Setări" [r_logs]="Jurnal" [r_upd]="Actualizare"
  [r_plain]="Fără HTTPS, conectarea circulă necriptat (vocea rămâne criptată). Cu un domeniu e mai sigur."
  [r_dns]="Dacă HTTPS nu merge într-un minut, verifică dacă numele arată spre acest server și dacă TCP 80/443 sunt deschise."
  [nothing]="Nimic instalat încă: instalez cu valorile implicite"
  [root]="Rulează ca root: sudo bash install.sh"
  [enter]="Enter păstrează valoarea din paranteze."
)
declare -A L_es=(
  [q_auto]="Actualizaciones automáticas del servidor (a diario, hacia las 4 de la mañana)" [autodone]="Actualizaciones automáticas: activadas"
  [title]="Spectre Voice Server — instalación"
  [step]="Paso %s de %s · %s"
  [s_lang]="Idioma" [s_name]="Nombre, contraseña y contacto" [s_net]="Red" [s_port]="Puerto" [s_extra]="Extras" [s_fw]="Cortafuegos" [s_inst]="Instalación"
  [q_name]="Nombre del servidor"
  [q_contact]="Tu e-mail para las solicitudes de datos (se muestra en /privacy)"
  [q_pass]="Contraseña del servidor (vacía: entra cualquiera con la dirección; los admins pueden cambiarla luego en la app)"
  [net_q]="¿Cómo llegará la gente a este servidor?"
  [net_1]="Mi propio dominio, con HTTPS (p. ej. voice.example.com)"
  [net_2]="Un nombre automático gratuito, con HTTPS: %s"
  [net_3]="La IP pública y un puerto, sin HTTPS"
  [net_4]="Solo mi red local (LAN)"
  [choice]="Opción"
  [bad]="Elige uno de los números de arriba."
  [q_domain]="Dominio (su DNS ya debe apuntar a %s)"
  [q_email]="E-mail para el certificado HTTPS (Let's Encrypt)"
  [q_public]="IP pública (o nombre DDNS) a la que llegan las apps para la voz"
  [q_lan]="Dirección LAN de este servidor"
  [q_port]="Puerto para las apps y la voz (TCP y UDP)"
  [q_vport]="Puerto para la voz (UDP)"
  [q_turn]="Relé TURN para redes que bloquean UDP (coturn)"
  [q_music]="Bot de música (!play: instala ffmpeg y yt-dlp)"
  [q_fw]="Abrir estos puertos en el cortafuegos (%s)"
  [no_fw]="No hay cortafuegos activo (ufw/firewalld). Si tu proveedor tiene uno en su panel, abre allí los puertos."
  [yn]="S/n" [ny]="s/N"
  [pkgs]="Paquetes" [dl]="Descargando el servidor" [build]="No hay versión para descargar: compilando desde el código"
  [music]="Bot de música: ffmpeg y yt-dlp" [https]="HTTPS para %s (Caddy)" [turn]="Relé TURN (coturn)" [fwdone]="Cortafuegos: puertos abiertos"
  [start]="Iniciando el servidor" [fail]="El servidor no arrancó; mira arriba."
  [ready]="Spectre Voice Server %s está en marcha"
  [r_addr]="Dirección del servidor" [r_port]="Puerto" [r_key]="Clave de administrador"
  [r_how]="En la app: Servidor › Reclamar y escribe la clave. Te convierte en el dueño del servidor (sirve una vez)."
  [r_owned]="(el servidor ya tiene dueño)"
  [r_pass]="Contraseña del servidor" [r_open]="ninguna (servidor abierto)"
  [r_ports]="Puertos a abrir" [r_set]="Ajustes" [r_logs]="Registro" [r_upd]="Actualizar"
  [r_plain]="Sin HTTPS el inicio de sesión viaja sin cifrar (la voz sigue cifrada). Un dominio es más seguro."
  [r_dns]="Si HTTPS no funciona en un minuto, comprueba que el nombre apunta a este servidor y que TCP 80/443 están abiertos."
  [nothing]="Nada instalado aún: instalando con los valores por defecto"
  [root]="Ejecuta como root: sudo bash install.sh"
  [enter]="Enter mantiene el valor entre corchetes."
)
declare -A L_de=(
  [q_auto]="Automatische Server-Updates (täglich, gegen 4 Uhr morgens)" [autodone]="Automatische Updates: an"
  [title]="Spectre Voice Server — Installation"
  [step]="Schritt %s von %s · %s"
  [s_lang]="Sprache" [s_name]="Name, Passwort und Kontakt" [s_net]="Netzwerk" [s_port]="Port" [s_extra]="Extras" [s_fw]="Firewall" [s_inst]="Installation"
  [q_name]="Name des Servers"
  [q_contact]="Deine E-Mail für Datenanfragen (wird unter /privacy angezeigt)"
  [q_pass]="Server-Passwort (leer: jeder mit der Adresse kommt rein; Admins können es später in der App ändern)"
  [net_q]="Wie erreichen die Leute diesen Server?"
  [net_1]="Meine eigene Domain, mit HTTPS (z. B. voice.example.com)"
  [net_2]="Ein kostenloser automatischer Name, mit HTTPS: %s"
  [net_3]="Die öffentliche IP und ein Port, ohne HTTPS"
  [net_4]="Nur mein lokales Netzwerk (LAN)"
  [choice]="Auswahl"
  [bad]="Wähle eine der Zahlen oben."
  [q_domain]="Domain (ihr DNS muss bereits auf %s zeigen)"
  [q_email]="E-Mail für das HTTPS-Zertifikat (Let's Encrypt)"
  [q_public]="Öffentliche IP (oder DDNS-Name), die die Apps für die Sprache erreichen"
  [q_lan]="LAN-Adresse dieses Servers"
  [q_port]="Port für die Apps und die Sprache (TCP und UDP)"
  [q_vport]="Port für die Sprache (UDP)"
  [q_turn]="TURN-Relay für Netze, die UDP sperren (coturn)"
  [q_music]="Musikbot (!play: installiert ffmpeg und yt-dlp)"
  [q_fw]="Diese Ports in der Firewall öffnen (%s)"
  [no_fw]="Keine aktive Firewall gefunden (ufw/firewalld). Hat dein Anbieter eine im Panel, öffne die Ports dort."
  [yn]="J/n" [ny]="j/N"
  [pkgs]="Pakete" [dl]="Server wird heruntergeladen" [build]="Keine Version zum Herunterladen: wird aus dem Quellcode gebaut"
  [music]="Musikbot: ffmpeg und yt-dlp" [https]="HTTPS für %s (Caddy)" [turn]="TURN-Relay (coturn)" [fwdone]="Firewall: Ports geöffnet"
  [start]="Server wird gestartet" [fail]="Der Server ist nicht gestartet; siehe oben."
  [ready]="Spectre Voice Server %s läuft"
  [r_addr]="Serveradresse" [r_port]="Port" [r_key]="Admin-Schlüssel"
  [r_how]="In der App: Server › Beanspruchen, dann den Admin-Schlüssel eingeben. Er macht dich zum Besitzer (einmalig)."
  [r_owned]="(der Server hat schon einen Besitzer)"
  [r_pass]="Server-Passwort" [r_open]="keins (offener Server)"
  [r_ports]="Zu öffnende Ports" [r_set]="Einstellungen" [r_logs]="Protokoll" [r_upd]="Aktualisieren"
  [r_plain]="Ohne HTTPS wird die Anmeldung unverschlüsselt übertragen (die Sprache bleibt verschlüsselt). Eine Domain ist sicherer."
  [r_dns]="Kommt HTTPS nicht binnen einer Minute, prüfe, ob der Name auf diesen Server zeigt und TCP 80/443 offen sind."
  [nothing]="Noch nichts installiert: Installation mit Standardwerten"
  [root]="Als root ausführen: sudo bash install.sh"
  [enter]="Enter behält den Wert in Klammern."
)
declare -A L_pl=(
  [q_auto]="Automatyczne aktualizacje serwera (codziennie, około 4 rano)" [autodone]="Automatyczne aktualizacje: włączone"
  [title]="Spectre Voice Server — instalacja"
  [step]="Krok %s z %s · %s"
  [s_lang]="Język" [s_name]="Nazwa, hasło i kontakt" [s_net]="Sieć" [s_port]="Port" [s_extra]="Dodatki" [s_fw]="Zapora" [s_inst]="Instalacja"
  [q_name]="Nazwa serwera"
  [q_contact]="Twój e-mail do wniosków o dane (widoczny pod /privacy)"
  [q_pass]="Hasło serwera (puste: wejdzie każdy z adresem; admini mogą je później zmienić w aplikacji)"
  [net_q]="Jak ludzie dotrą do tego serwera?"
  [net_1]="Moja własna domena, z HTTPS (np. voice.example.com)"
  [net_2]="Darmowa automatyczna nazwa, z HTTPS: %s"
  [net_3]="Publiczny IP i port, bez HTTPS"
  [net_4]="Tylko moja sieć lokalna (LAN)"
  [choice]="Wybór"
  [bad]="Wybierz jedną z liczb powyżej."
  [q_domain]="Domena (jej DNS musi już wskazywać na %s)"
  [q_email]="E-mail do certyfikatu HTTPS (Let's Encrypt)"
  [q_public]="Publiczny IP (lub nazwa DDNS), pod który łączą się aplikacje dla głosu"
  [q_lan]="Adres LAN tego serwera"
  [q_port]="Port dla aplikacji i głosu (TCP i UDP)"
  [q_vport]="Port dla głosu (UDP)"
  [q_turn]="Przekaźnik TURN dla sieci blokujących UDP (coturn)"
  [q_music]="Bot muzyczny (!play: instaluje ffmpeg i yt-dlp)"
  [q_fw]="Otworzyć te porty w zaporze (%s)"
  [no_fw]="Brak aktywnej zapory (ufw/firewalld). Jeśli dostawca ma ją w panelu, otwórz porty tam."
  [yn]="T/n" [ny]="t/N"
  [pkgs]="Pakiety" [dl]="Pobieranie serwera" [build]="Brak wersji do pobrania: kompilacja ze źródeł"
  [music]="Bot muzyczny: ffmpeg i yt-dlp" [https]="HTTPS dla %s (Caddy)" [turn]="Przekaźnik TURN (coturn)" [fwdone]="Zapora: porty otwarte"
  [start]="Uruchamianie serwera" [fail]="Serwer się nie uruchomił; zobacz wyżej."
  [ready]="Spectre Voice Server %s działa"
  [r_addr]="Adres serwera" [r_port]="Port" [r_key]="Klucz administratora"
  [r_how]="W aplikacji: Serwer › Przejmij, potem wpisz klucz. Zostaniesz właścicielem serwera (działa raz)."
  [r_owned]="(serwer ma już właściciela)"
  [r_pass]="Hasło serwera" [r_open]="brak (serwer otwarty)"
  [r_ports]="Porty do otwarcia" [r_set]="Ustawienia" [r_logs]="Logi" [r_upd]="Aktualizacja"
  [r_plain]="Bez HTTPS logowanie idzie niezaszyfrowane (głos pozostaje szyfrowany). Domena jest bezpieczniejsza."
  [r_dns]="Jeśli HTTPS nie zadziała w minutę, sprawdź, czy nazwa wskazuje na ten serwer i czy TCP 80/443 są otwarte."
  [nothing]="Nic jeszcze nie zainstalowano: instalacja z ustawieniami domyślnymi"
  [root]="Uruchom jako root: sudo bash install.sh"
  [enter]="Enter zostawia wartość w nawiasach."
)
declare -A L_fr=(
  [q_auto]="Mises à jour automatiques du serveur (chaque jour, vers 4 h du matin)" [autodone]="Mises à jour automatiques : activées"
  [title]="Spectre Voice Server — installation"
  [step]="Étape %s sur %s · %s"
  [s_lang]="Langue" [s_name]="Nom, mot de passe et contact" [s_net]="Réseau" [s_port]="Port" [s_extra]="Options" [s_fw]="Pare-feu" [s_inst]="Installation"
  [q_name]="Nom du serveur"
  [q_contact]="Ton e-mail pour les demandes sur les données (affiché sur /privacy)"
  [q_pass]="Mot de passe du serveur (vide : quiconque a l'adresse entre ; les admins peuvent le changer ensuite dans l'appli)"
  [net_q]="Comment les gens rejoindront-ils ce serveur ?"
  [net_1]="Mon propre domaine, avec HTTPS (ex. voice.example.com)"
  [net_2]="Un nom automatique gratuit, avec HTTPS : %s"
  [net_3]="L'IP publique et un port, sans HTTPS"
  [net_4]="Seulement mon réseau local (LAN)"
  [choice]="Choix"
  [bad]="Choisis un des numéros ci-dessus."
  [q_domain]="Domaine (son DNS doit déjà pointer vers %s)"
  [q_email]="E-mail pour le certificat HTTPS (Let's Encrypt)"
  [q_public]="IP publique (ou nom DDNS) que les apps joignent pour la voix"
  [q_lan]="Adresse LAN de ce serveur"
  [q_port]="Port pour les apps et la voix (TCP et UDP)"
  [q_vport]="Port pour la voix (UDP)"
  [q_turn]="Relais TURN pour les réseaux qui bloquent l'UDP (coturn)"
  [q_music]="Bot musical (!play : installe ffmpeg et yt-dlp)"
  [q_fw]="Ouvrir ces ports dans le pare-feu (%s)"
  [no_fw]="Aucun pare-feu actif ici (ufw/firewalld). Si ton hébergeur en a un dans son panneau, ouvre les ports là-bas."
  [yn]="O/n" [ny]="o/N"
  [pkgs]="Paquets" [dl]="Téléchargement du serveur" [build]="Aucune version à télécharger : compilation depuis les sources"
  [music]="Bot musical : ffmpeg et yt-dlp" [https]="HTTPS pour %s (Caddy)" [turn]="Relais TURN (coturn)" [fwdone]="Pare-feu : ports ouverts"
  [start]="Démarrage du serveur" [fail]="Le serveur n'a pas démarré ; voir ci-dessus."
  [ready]="Spectre Voice Server %s fonctionne"
  [r_addr]="Adresse du serveur" [r_port]="Port" [r_key]="Clé d'administration"
  [r_how]="Dans l'app : Serveur › Revendiquer, puis tape la clé. Elle fait de toi le propriétaire (une seule fois)."
  [r_owned]="(le serveur a déjà un propriétaire)"
  [r_pass]="Mot de passe du serveur" [r_open]="aucun (serveur ouvert)"
  [r_ports]="Ports à ouvrir" [r_set]="Réglages" [r_logs]="Journal" [r_upd]="Mise à jour"
  [r_plain]="Sans HTTPS, la connexion passe en clair (la voix reste chiffrée). Un domaine est plus sûr."
  [r_dns]="Si HTTPS ne marche pas d'ici une minute, vérifie que le nom pointe vers ce serveur et que TCP 80/443 sont ouverts."
  [nothing]="Rien d'installé encore : installation avec les valeurs par défaut"
  [root]="Lance-le en root : sudo bash install.sh"
  [enter]="Entrée garde la valeur entre crochets."
)
declare -A L_it=(
  [q_auto]="Aggiornamenti automatici del server (ogni giorno, verso le 4 del mattino)" [autodone]="Aggiornamenti automatici: attivi"
  [title]="Spectre Voice Server — installazione"
  [step]="Passo %s di %s · %s"
  [s_lang]="Lingua" [s_name]="Nome, password e contatto" [s_net]="Rete" [s_port]="Porta" [s_extra]="Extra" [s_fw]="Firewall" [s_inst]="Installazione"
  [q_name]="Nome del server"
  [q_contact]="La tua e-mail per le richieste sui dati (mostrata su /privacy)"
  [q_pass]="Password del server (vuota: entra chiunque abbia l'indirizzo; gli admin possono cambiarla poi nell'app)"
  [net_q]="Come raggiungeranno questo server le persone?"
  [net_1]="Il mio dominio, con HTTPS (es. voice.example.com)"
  [net_2]="Un nome automatico gratuito, con HTTPS: %s"
  [net_3]="L'IP pubblico e una porta, senza HTTPS"
  [net_4]="Solo la mia rete locale (LAN)"
  [choice]="Scelta"
  [bad]="Scegli uno dei numeri sopra."
  [q_domain]="Dominio (il suo DNS deve già puntare a %s)"
  [q_email]="E-mail per il certificato HTTPS (Let's Encrypt)"
  [q_public]="IP pubblico (o nome DDNS) che le app raggiungono per la voce"
  [q_lan]="Indirizzo LAN di questo server"
  [q_port]="Porta per le app e la voce (TCP e UDP)"
  [q_vport]="Porta per la voce (UDP)"
  [q_turn]="Relay TURN per le reti che bloccano UDP (coturn)"
  [q_music]="Bot musicale (!play: installa ffmpeg e yt-dlp)"
  [q_fw]="Aprire queste porte nel firewall (%s)"
  [no_fw]="Nessun firewall attivo qui (ufw/firewalld). Se il tuo provider ne ha uno nel pannello, apri lì le porte."
  [yn]="S/n" [ny]="s/N"
  [pkgs]="Pacchetti" [dl]="Scarico il server" [build]="Nessuna versione da scaricare: compilo dai sorgenti"
  [music]="Bot musicale: ffmpeg e yt-dlp" [https]="HTTPS per %s (Caddy)" [turn]="Relay TURN (coturn)" [fwdone]="Firewall: porte aperte"
  [start]="Avvio del server" [fail]="Il server non è partito; vedi sopra."
  [ready]="Spectre Voice Server %s è in funzione"
  [r_addr]="Indirizzo del server" [r_port]="Porta" [r_key]="Chiave di amministrazione"
  [r_how]="Nell'app: Server › Rivendica, poi scrivi la chiave. Ti rende proprietario del server (vale una volta)."
  [r_owned]="(il server ha già un proprietario)"
  [r_pass]="Password del server" [r_open]="nessuna (server aperto)"
  [r_ports]="Porte da aprire" [r_set]="Impostazioni" [r_logs]="Log" [r_upd]="Aggiornamento"
  [r_plain]="Senza HTTPS l'accesso viaggia in chiaro (la voce resta cifrata). Un dominio è più sicuro."
  [r_dns]="Se HTTPS non funziona entro un minuto, controlla che il nome punti a questo server e che TCP 80/443 siano aperte."
  [nothing]="Ancora nulla installato: installo con i valori predefiniti"
  [root]="Esegui come root: sudo bash install.sh"
  [enter]="Invio mantiene il valore tra parentesi."
)
declare -A L_pt=(
  [q_auto]="Atualizações automáticas do servidor (diariamente, por volta das 4 da manhã)" [autodone]="Atualizações automáticas: ativadas"
  [title]="Spectre Voice Server — instalação"
  [step]="Passo %s de %s · %s"
  [s_lang]="Idioma" [s_name]="Nome, palavra-passe e contacto" [s_net]="Rede" [s_port]="Porta" [s_extra]="Extras" [s_fw]="Firewall" [s_inst]="Instalação"
  [q_name]="Nome do servidor"
  [q_contact]="O teu e-mail para pedidos sobre dados (aparece em /privacy)"
  [q_pass]="Palavra-passe do servidor (vazia: entra quem tiver o endereço; os admins podem mudá-la depois na app)"
  [net_q]="Como é que as pessoas chegam a este servidor?"
  [net_1]="O meu próprio domínio, com HTTPS (ex. voice.example.com)"
  [net_2]="Um nome automático gratuito, com HTTPS: %s"
  [net_3]="O IP público e uma porta, sem HTTPS"
  [net_4]="Só a minha rede local (LAN)"
  [choice]="Escolha"
  [bad]="Escolhe um dos números acima."
  [q_domain]="Domínio (o DNS já tem de apontar para %s)"
  [q_email]="E-mail para o certificado HTTPS (Let's Encrypt)"
  [q_public]="IP público (ou nome DDNS) a que as apps chegam para a voz"
  [q_lan]="Endereço LAN deste servidor"
  [q_port]="Porta para as apps e a voz (TCP e UDP)"
  [q_vport]="Porta para a voz (UDP)"
  [q_turn]="Relé TURN para redes que bloqueiam UDP (coturn)"
  [q_music]="Bot de música (!play: instala ffmpeg e yt-dlp)"
  [q_fw]="Abrir estas portas na firewall (%s)"
  [no_fw]="Nenhuma firewall ativa aqui (ufw/firewalld). Se o teu fornecedor tiver uma no painel, abre lá as portas."
  [yn]="S/n" [ny]="s/N"
  [pkgs]="Pacotes" [dl]="A transferir o servidor" [build]="Nenhuma versão para transferir: a compilar do código"
  [music]="Bot de música: ffmpeg e yt-dlp" [https]="HTTPS para %s (Caddy)" [turn]="Relé TURN (coturn)" [fwdone]="Firewall: portas abertas"
  [start]="A iniciar o servidor" [fail]="O servidor não arrancou; vê acima."
  [ready]="Spectre Voice Server %s está a funcionar"
  [r_addr]="Endereço do servidor" [r_port]="Porta" [r_key]="Chave de administrador"
  [r_how]="Na app: Servidor › Reclamar e escreve a chave. Torna-te o dono do servidor (só uma vez)."
  [r_owned]="(o servidor já tem dono)"
  [r_pass]="Palavra-passe do servidor" [r_open]="nenhuma (servidor aberto)"
  [r_ports]="Portas a abrir" [r_set]="Definições" [r_logs]="Registos" [r_upd]="Atualizar"
  [r_plain]="Sem HTTPS a autenticação viaja sem cifra (a voz continua cifrada). Um domínio é mais seguro."
  [r_dns]="Se o HTTPS não funcionar num minuto, confirma que o nome aponta para este servidor e que TCP 80/443 estão abertas."
  [nothing]="Nada instalado ainda: a instalar com os valores por omissão"
  [root]="Executa como root: sudo bash install.sh"
  [enter]="Enter mantém o valor entre parênteses."
)
declare -A L_hu=(
  [q_auto]="Automatikus szerverfrissítések (naponta, hajnali 4 körül)" [autodone]="Automatikus frissítések: bekapcsolva"
  [title]="Spectre Voice Server — telepítés"
  [step]="%s. lépés / %s · %s"
  [s_lang]="Nyelv" [s_name]="Név, jelszó és kapcsolat" [s_net]="Hálózat" [s_port]="Port" [s_extra]="Extrák" [s_fw]="Tűzfal" [s_inst]="Telepítés"
  [q_name]="A szerver neve"
  [q_contact]="E-mail címed az adatkérésekhez (a /privacy oldalon látszik)"
  [q_pass]="Szerverjelszó (üres: bárki beléphet, akinek megvan a cím; az adminok később az appban módosíthatják)"
  [net_q]="Hogyan érik el az emberek ezt a szervert?"
  [net_1]="Saját domain, HTTPS-sel (pl. voice.example.com)"
  [net_2]="Ingyenes automatikus név, HTTPS-sel: %s"
  [net_3]="A nyilvános IP és egy port, HTTPS nélkül"
  [net_4]="Csak a helyi hálózatom (LAN)"
  [choice]="Választás"
  [bad]="Válassz egyet a fenti számok közül."
  [q_domain]="Domain (a DNS-ének már %s címre kell mutatnia)"
  [q_email]="E-mail a HTTPS tanúsítványhoz (Let's Encrypt)"
  [q_public]="Nyilvános IP (vagy DDNS név), amit az appok a hanghoz elérnek"
  [q_lan]="A szerver LAN címe"
  [q_port]="Port az appokhoz és a hanghoz (TCP és UDP)"
  [q_vport]="Port a hanghoz (UDP)"
  [q_turn]="TURN relé az UDP-t tiltó hálózatokhoz (coturn)"
  [q_music]="Zenebot (!play: ffmpeg-et és yt-dlp-t telepít)"
  [q_fw]="Megnyissam ezeket a portokat a tűzfalon (%s)"
  [no_fw]="Nincs aktív tűzfal (ufw/firewalld). Ha a szolgáltatód paneljén van, ott nyisd meg a portokat."
  [yn]="I/n" [ny]="i/N"
  [pkgs]="Csomagok" [dl]="A szerver letöltése" [build]="Nincs letölthető verzió: fordítás forrásból"
  [music]="Zenebot: ffmpeg és yt-dlp" [https]="HTTPS ehhez: %s (Caddy)" [turn]="TURN relé (coturn)" [fwdone]="Tűzfal: portok megnyitva"
  [start]="A szerver indítása" [fail]="A szerver nem indult el; lásd fent."
  [ready]="A Spectre Voice Server %s fut"
  [r_addr]="Szervercím" [r_port]="Port" [r_key]="Admin kulcs"
  [r_how]="Az appban: Szerver › Igénylés, majd írd be a kulcsot. Ezzel te leszel a tulajdonos (egyszer működik)."
  [r_owned]="(a szervernek már van tulajdonosa)"
  [r_pass]="Szerverjelszó" [r_open]="nincs (nyitott szerver)"
  [r_ports]="Megnyitandó portok" [r_set]="Beállítások" [r_logs]="Napló" [r_upd]="Frissítés"
  [r_plain]="HTTPS nélkül a bejelentkezés titkosítatlanul megy (a hang titkosított marad). Egy domain biztonságosabb."
  [r_dns]="Ha a HTTPS egy percen belül nem működik, ellenőrizd, hogy a név erre a szerverre mutat, és a TCP 80/443 nyitva van."
  [nothing]="Még nincs semmi telepítve: telepítés alapértékekkel"
  [root]="Futtasd rootként: sudo bash install.sh"
  [enter]="Az Enter megtartja a zárójeles értéket."
)
declare -A L_tr=(
  [q_auto]="Otomatik sunucu güncellemeleri (her gün, sabah 4 civarı)" [autodone]="Otomatik güncellemeler: açık"
  [title]="Spectre Voice Server — kurulum"
  [step]="Adım %s / %s · %s"
  [s_lang]="Dil" [s_name]="Ad, şifre ve iletişim" [s_net]="Ağ" [s_port]="Port" [s_extra]="Ekstralar" [s_fw]="Güvenlik duvarı" [s_inst]="Kurulum"
  [q_name]="Sunucunun adı"
  [q_contact]="Veri talepleri için e-postan (/privacy sayfasında görünür)"
  [q_pass]="Sunucu şifresi (boş: adresi bilen herkes girer; yöneticiler sonra uygulamadan değiştirebilir)"
  [net_q]="İnsanlar bu sunucuya nasıl ulaşacak?"
  [net_1]="Kendi alan adım, HTTPS ile (örn. voice.example.com)"
  [net_2]="Ücretsiz otomatik bir ad, HTTPS ile: %s"
  [net_3]="Genel IP ve bir port, HTTPS olmadan"
  [net_4]="Yalnızca yerel ağım (LAN)"
  [choice]="Seçim"
  [bad]="Yukarıdaki numaralardan birini seç."
  [q_domain]="Alan adı (DNS'i zaten %s adresini göstermeli)"
  [q_email]="HTTPS sertifikası için e-posta (Let's Encrypt)"
  [q_public]="Uygulamaların ses için eriştiği genel IP (veya DDNS adı)"
  [q_lan]="Bu sunucunun LAN adresi"
  [q_port]="Uygulamalar ve ses için port (TCP ve UDP)"
  [q_vport]="Ses için port (UDP)"
  [q_turn]="UDP'yi engelleyen ağlar için TURN aktarıcı (coturn)"
  [q_music]="Müzik botu (!play: ffmpeg ve yt-dlp kurar)"
  [q_fw]="Bu portları güvenlik duvarında aç (%s)"
  [no_fw]="Etkin güvenlik duvarı yok (ufw/firewalld). Sağlayıcının panelinde varsa portları orada aç."
  [yn]="E/h" [ny]="e/H"
  [pkgs]="Paketler" [dl]="Sunucu indiriliyor" [build]="İndirilecek sürüm yok: kaynaktan derleniyor"
  [music]="Müzik botu: ffmpeg ve yt-dlp" [https]="%s için HTTPS (Caddy)" [turn]="TURN aktarıcı (coturn)" [fwdone]="Güvenlik duvarı: portlar açıldı"
  [start]="Sunucu başlatılıyor" [fail]="Sunucu başlamadı; yukarıya bak."
  [ready]="Spectre Voice Server %s çalışıyor"
  [r_addr]="Sunucu adresi" [r_port]="Port" [r_key]="Yönetici anahtarı"
  [r_how]="Uygulamada: Sunucu › Sahiplen, sonra anahtarı yaz. Seni sunucunun sahibi yapar (bir kez çalışır)."
  [r_owned]="(sunucunun zaten bir sahibi var)"
  [r_pass]="Sunucu şifresi" [r_open]="yok (açık sunucu)"
  [r_ports]="Açılacak portlar" [r_set]="Ayarlar" [r_logs]="Kayıtlar" [r_upd]="Güncelleme"
  [r_plain]="HTTPS olmadan oturum açma şifresiz gider (ses şifreli kalır). Alan adı daha güvenlidir."
  [r_dns]="HTTPS bir dakika içinde çalışmazsa, adın bu sunucuyu gösterdiğini ve TCP 80/443'ün açık olduğunu kontrol et."
  [nothing]="Henüz bir şey kurulu değil: varsayılanlarla kuruluyor"
  [root]="root olarak çalıştır: sudo bash install.sh"
  [enter]="Enter köşeli parantezdeki değeri korur."
)
LANGS=(en ro es de pl fr it pt hu tr)
LANGNAMES=("English" "Română" "Español" "Deutsch" "Polski" "Français" "Italiano" "Português" "Magyar" "Türkçe")

t() { # t KEY: the text in the chosen language
  local -n d="L_${LANGSEL:-en}"
  local v=${d[$1]:-}
  [[ -n $v ]] || v=${L_en[$1]:-$1}
  printf '%s' "$v"
}

say()  { printf '\033[1;36m▸ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m! %s\033[0m\n' "$*"; }
TOTAL=7
stepn() { echo; printf '\033[1;35m━━ '; printf "$(t step)" "$1" "$TOTAL" "$2"; printf ' ━━\033[0m\n'; }
interactive() { [[ $YES -eq 0 && ( -t 0 || -e /dev/tty ) ]]; }
ask() { # ask VAR "question" default
  local var=$1 q=$2 def=${3:-} a=""
  if ! interactive; then printf -v "$var" '%s' "${!var:-$def}"; return; fi
  [[ -n "${!var:-}" ]] && def=${!var}
  read -rp "  $q [${def}]: " a </dev/tty || true
  printf -v "$var" '%s' "${a:-$def}"
}
yesno() { # yesno VAR "question" 1|0
  local var=$1 q=$2 def=$3 a=""
  if ! interactive; then return; fi
  read -rp "  $q [$([[ $def -eq 1 ]] && t yn || t ny)]: " a </dev/tty || true
  case "${a,,}" in
    y|yes|d|da|s|si|sí|sim|j|ja|t|tak|o|oui|i|igen|e|evet) printf -v "$var" 1 ;;
    n|no|nu|nein|nie|non|não|nao|nem|h|hayır|hayir) printf -v "$var" 0 ;;
    *) printf -v "$var" '%s' "$def" ;;
  esac
}
choose() { # choose VAR max default
  local var=$1 max=$2 def=$3 a=""
  if ! interactive; then printf -v "$var" '%s' "${!var:-$def}"; return; fi
  while true; do
    read -rp "  $(t choice) [${def}]: " a </dev/tty || true
    a=${a:-$def}
    if [[ $a =~ ^[0-9]+$ ]] && ((a >= 1 && a <= max)); then printf -v "$var" '%s' "$a"; return; fi
    warn "$(t bad)"
  done
}
setenv() { # setenv KEY VALUE (in server.env)
  local k=$1 v=$2
  local e=${v//\\/\\\\}; e=${e//|/\\|}; e=${e//&/\\&} # sed's special characters in the value
  if grep -qE "^#?\s*${k}=" "$ENVF"; then sed -i -E "s|^#?\s*${k}=.*|${k}=${e}|" "$ENVF"; else printf '%s=%s\n' "$k" "$v" >>"$ENVF"; fi
}
row() { # row LABEL VALUE [color]: the label padded by characters, not bytes (ă, ü, ł…)
  local LC_ALL=C.UTF-8 l=$1 v=$2 c=${3:-}
  local n=$((22 - ${#l})); ((n < 1)) && n=1
  printf '  %s%*s%b%s\033[0m\n' "$l" "$n" '' "$c" "$v"
}
isprivate() { [[ $1 =~ ^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|127\.) ]]; }

[[ $EUID -eq 0 ]] || { echo "$(t root)"; exit 1; }
command -v apt-get >/dev/null || { echo "Debian or Ubuntu needed (apt-get)."; exit 1; }
ARCH=$(dpkg --print-architecture)
case "$ARCH" in amd64|arm64) ;; *) echo "Unsupported CPU: $ARCH (amd64 or arm64)"; exit 1 ;; esac
HERE=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo /tmp)
grep -qs "spectrevoice-server" "$HERE/go.mod" || HERE=/nonexistent # run through curl: nothing local
FIRST=0
[[ -f $ENVF ]] || FIRST=1

# ---- Step 1: the language ----
if [[ -z $LANGSEL && $FIRST -eq 1 ]] && interactive; then
  printf '\n\033[1;35m%s\033[0m\n' "Spectre Voice Server"
  stepn 1 "Language · Limba · Idioma · Sprache · Język"
  for i in "${!LANGS[@]}"; do printf '   %2d) %s\n' $((i + 1)) "${LANGNAMES[$i]}"; done
  n=""
  choose n ${#LANGS[@]} 1
  LANGSEL=${LANGS[$((n - 1))]}
fi
[[ " ${LANGS[*]} " == *" ${LANGSEL:-en} "* ]] || LANGSEL=en
LANGSEL=${LANGSEL:-en}
interactive && [[ $FIRST -eq 1 ]] && { printf '\n\033[1m%s\033[0m\n' "$(t title)"; echo "  $(t enter)"; }

say "$(t pkgs)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates tar iproute2 >/dev/null

LANIP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' | head -1)
[[ $UPDATE -eq 1 && $FIRST -eq 1 ]] && warn "$(t nothing)"
# An update keeps what was chosen: the music bot only if it's on, automatic updates unless they were turned off
if [[ $FIRST -eq 0 ]]; then
  if grep -q "^SV_MUSIC=true" "$ENVF"; then MUSIC=1; else MUSIC=0; fi
  grep -q "^SV_AUTO_UPDATE=false" "$ENVF" && AUTOUPD=0
fi

if [[ $FIRST -eq 1 ]]; then
  WANIP=$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || true)
  # ---- Step 2: name and contact ----
  stepn 2 "$(t s_name)"
  ask NAME "$(t q_name)" "Spectre Voice"
  # One server is one space (as on TeamSpeak): the address, a name, and this password if there is one
  ask SVPASS "$(t q_pass)" ""
  # You hold the people's data on your server (GDPR): your privacy page (/privacy) gives this address for their requests
  ask CONTACT "$(t q_contact)" "${EMAIL:-}"

  # ---- Step 3: the network ----
  stepn 3 "$(t s_net)"
  FREE=""
  [[ -n $WANIP ]] && FREE="${WANIP//./-}.sslip.io"
  if [[ -n $DOMAIN ]]; then MODE=1
  elif [[ -n $PUBLIC && -n $PORT ]]; then MODE=3
  elif [[ $YES -eq 1 ]]; then MODE=4
  else
    echo "  $(t net_q)"
    echo "   1) $(t net_1)"
    if [[ -n $FREE ]]; then printf '   2) '; printf "$(t net_2)" "$FREE"; echo; fi
    echo "   3) $(t net_3)"
    echo "   4) $(t net_4)"
    # A machine with a public address (a VPS): the free name; at home: the LAN
    def=4
    if [[ -n $WANIP && -n $LANIP ]] && ! isprivate "$LANIP"; then def=2; fi
    [[ -z $FREE && $def -eq 2 ]] && def=3
    choose MODE 4 "$def"
    [[ $MODE -eq 2 && -z $FREE ]] && MODE=3
  fi
  case $MODE in
    1)
      ask DOMAIN "$(printf "$(t q_domain)" "${WANIP:-?}")" ""
      [[ -n $DOMAIN ]] || MODE=3
      ;;
    2) DOMAIN=$FREE ;;
  esac
  if [[ $MODE -le 2 ]]; then
    ask EMAIL "$(t q_email)" "${CONTACT:-}"
    ask PUBLIC "$(t q_public)" "${WANIP:-$DOMAIN}"
  elif [[ $MODE -eq 3 ]]; then
    ask PUBLIC "$(t q_public)" "${WANIP:-$LANIP}"
    TURN=0
  else
    ask PUBLIC "$(t q_lan)" "$LANIP"
    TURN=0
  fi

  # ---- Step 4: the port ----
  stepn 4 "$(t s_port)"
  if [[ $MODE -le 2 ]]; then ask PORT "$(t q_vport)" "${PORT:-7880}"; else ask PORT "$(t q_port)" "${PORT:-7880}"; fi
  [[ $PORT =~ ^[0-9]+$ ]] && ((PORT >= 1024 && PORT <= 65535)) || PORT=7880

  # ---- Step 5: extras ----
  stepn 5 "$(t s_extra)"
  [[ $MODE -le 2 ]] && yesno TURN "$(t q_turn)" "$TURN"
  yesno MUSIC "$(t q_music)" "$MUSIC"
  yesno AUTOUPD "$(t q_auto)" "$AUTOUPD"

  # ---- Step 6: the firewall ----
  stepn 6 "$(t s_fw)"
  OPEN_TCP="" OPEN_UDP=""
  if [[ $MODE -le 2 ]]; then OPEN_TCP="80 443"; else OPEN_TCP="$PORT"; fi
  if [[ -n $UDP ]]; then OPEN_UDP="${UDP/-/:}"; else OPEN_UDP="$PORT"; fi
  if [[ $MODE -le 2 && $TURN -eq 1 ]]; then OPEN_TCP="$OPEN_TCP 3478"; OPEN_UDP="$OPEN_UDP 3478 49152:49352"; fi
  FW=""
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then FW=ufw
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then FW=firewalld; fi
  PORTS_TXT="TCP ${OPEN_TCP// /, } · UDP ${OPEN_UDP// /, }"
  PORTS_TXT=${PORTS_TXT//:/-}
  if [[ -n $FW && $FIREWALL -eq 1 ]]; then
    yesno FIREWALL "$(printf "$(t q_fw)" "$PORTS_TXT")" 1
  else
    echo "  $(t no_fw)"
    echo "  ${PORTS_TXT}"
    FIREWALL=0
  fi
fi

# ---- Step 7: installing ----
[[ $FIRST -eq 1 ]] && stepn 7 "$(t s_inst)"
TAG=main # the service file comes from the same release as the program
install_binary() {
  [[ $SOURCE -eq 1 ]] && return 1
  TAG=$(curl -fsSI "https://github.com/${REPO}/releases/latest" 2>/dev/null | awk -F/ 'tolower($1) ~ /^location:/ {print $NF}' | tr -d '\r' || true)
  [[ -n "$TAG" ]] || TAG=main
  local url="https://github.com/${REPO}/releases/latest/download/spectrevoice-server-linux-${ARCH}.tar.gz"
  local tmp; tmp=$(mktemp -d)
  if curl -fsSL "$url" -o "$tmp/s.tgz" && curl -fsSL "https://github.com/${REPO}/releases/latest/download/SHA256SUMS" -o "$tmp/SHA256SUMS"; then
    (cd "$tmp" && grep "linux-${ARCH}.tar.gz" SHA256SUMS | sed "s|spectrevoice-server-linux-${ARCH}.tar.gz|s.tgz|" | sha256sum -c --quiet) || { warn "checksum mismatch"; rm -rf "$tmp"; return 1; }
    tar -xzf "$tmp/s.tgz" -C "$tmp"
    install -m 755 "$tmp/spectrevoice-server" "$BIN.new" && mv -f "$BIN.new" "$BIN"
    rm -rf "$tmp"
    return 0
  fi
  rm -rf "$tmp"
  return 1
}
build_source() {
  local src="$HERE"
  if [[ ! -f "$src/go.mod" ]]; then
    apt-get install -y -qq git >/dev/null
    src=/opt/spectrevoice-server
    if [[ -d $src/.git ]]; then git -C "$src" pull -q --ff-only; else git clone -q --depth 1 "https://github.com/${REPO}.git" "$src"; fi
  fi
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    say "Go ${GO_VERSION}"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
  fi
  local ver; ver=$(git -C "$src" describe --tags --always 2>/dev/null || echo dev)
  (cd "$src" && CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -ldflags "-s -w -X main.version=$ver" -o /tmp/spectrevoice-server .)
  install -m 755 /tmp/spectrevoice-server "$BIN" && rm /tmp/spectrevoice-server
}
say "$(t dl)"
install_binary || { say "$(t build)"; build_source; }

# ---- User, folders, settings, service ----
id spectrevoice &>/dev/null || useradd --system --home "$DATA" --shell /usr/sbin/nologin spectrevoice
install -d -o spectrevoice -g spectrevoice -m 750 "$DATA"
install -d -o root -g spectrevoice -m 750 "$ETC"
if [[ $FIRST -eq 1 ]]; then
  if [[ -f "$HERE/deploy/server.env.example" ]]; then cp "$HERE/deploy/server.env.example" "$ENVF"
  else curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/deploy/server.env.example" -o "$ENVF"; fi
  chown root:spectrevoice "$ENVF" && chmod 640 "$ENVF"
  setenv SV_NAME "$NAME"
  setenv SV_PASSWORD "$SVPASS"
  [[ -n "$CONTACT" ]] && setenv SV_CONTACT_EMAIL "$CONTACT"
  setenv SV_PUBLIC_IP "$PUBLIC"
  setenv SV_MUSIC "$([[ $MUSIC -eq 1 ]] && echo true || echo false)"
  # Asked in step 5: "no" is kept, so a later --update or re-run doesn't turn them on again
  setenv SV_AUTO_UPDATE "$([[ $AUTOUPD -eq 1 ]] && echo true || echo false)"
  if [[ -n $UDP ]]; then
    setenv SV_UDP_MIN "${UDP%-*}"
    setenv SV_UDP_MAX "${UDP#*-}"
  else
    setenv SV_UDP_PORT "$PORT" # one UDP port for all the voice: one rule in the firewall
  fi
  if [[ -n "$DOMAIN" ]]; then
    setenv SV_INVITE_BASE "https://${DOMAIN}/i/"
    setenv SV_LISTEN "127.0.0.1:7880" # from outside only through Caddy (HTTPS)
  else
    setenv SV_LISTEN "0.0.0.0:${PORT}"
  fi
fi
unit=/etc/systemd/system/spectrevoice.service
if [[ -f "$HERE/deploy/spectrevoice.service" ]]; then cp "$HERE/deploy/spectrevoice.service" "$unit"
else curl -fsSL "https://raw.githubusercontent.com/${REPO}/${TAG}/deploy/spectrevoice.service" -o "$unit"; fi

# ---- Music bot ----
if [[ $MUSIC -eq 1 ]]; then
  say "$(t music)"
  apt-get install -y -qq ffmpeg >/dev/null
  ytarch=$([[ $ARCH == arm64 ]] && echo yt-dlp_linux_aarch64 || echo yt-dlp_linux)
  curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/latest/download/${ytarch}" -o /usr/local/bin/yt-dlp && chmod 755 /usr/local/bin/yt-dlp
  # YouTube breaks old yt-dlp within weeks: keep it new (daily)
  printf '[Unit]\nDescription=Update yt-dlp (Spectre Voice music bot)\n[Service]\nType=oneshot\nExecStart=/usr/local/bin/yt-dlp -U\n' >/etc/systemd/system/spectrevoice-ytdlp.service
  printf '[Unit]\nDescription=Update yt-dlp daily\n[Timer]\nOnCalendar=daily\nRandomizedDelaySec=6h\nPersistent=true\n[Install]\nWantedBy=timers.target\n' >/etc/systemd/system/spectrevoice-ytdlp.timer
  systemctl daemon-reload
  systemctl enable --now spectrevoice-ytdlp.timer >/dev/null 2>&1 || true
fi

# ---- HTTPS (Caddy) ----
if [[ $FIRST -eq 1 && -n "$DOMAIN" ]]; then
  say "$(printf "$(t https)" "$DOMAIN")"
  if ! command -v caddy >/dev/null; then
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg >/dev/null
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq && apt-get install -y -qq caddy >/dev/null
  fi
  # Our site in its own file; an existing Caddyfile (other sites) is kept and only imports it
  printf '%s {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:7880\n}\n' "$DOMAIN" >/etc/caddy/spectrevoice.caddy
  if [[ ! -s /etc/caddy/Caddyfile ]] || grep -q "The Caddyfile is an easy way" /etc/caddy/Caddyfile; then
    {
      [[ -n "$EMAIL" ]] && printf '{\n\temail %s\n}\n\n' "$EMAIL"
      echo "import /etc/caddy/spectrevoice.caddy"
    } >/etc/caddy/Caddyfile
  elif ! grep -q "spectrevoice.caddy" /etc/caddy/Caddyfile; then
    cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
    printf '\nimport /etc/caddy/spectrevoice.caddy\n' >>/etc/caddy/Caddyfile
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || true
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
fi

# ---- TURN (coturn) ----
if [[ $FIRST -eq 1 && $TURN -eq 1 && -n "$DOMAIN" ]]; then
  say "$(t turn)"
  apt-get install -y -qq coturn >/dev/null
  # A shared secret, not a user and password: the server gives each person credentials of their own that end
  # (SV_TURN_SECRET), so what an app once saw isn't a relay for anyone for ever
  tsecret=$(head -c 32 /dev/urandom | base64 | tr -d '/+=')
  cat >/etc/turnserver.conf <<EOF
listening-port=3478
fingerprint
use-auth-secret
static-auth-secret=${tsecret}
realm=${DOMAIN}
min-port=49152
max-port=49352
no-cli
no-tlsv1
no-tlsv1_1
no-multicast-peers
no-tcp-relay
# Relay only to the internet, never into this network
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=198.18.0.0-198.19.255.255
denied-peer-ip=224.0.0.0-255.255.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff
EOF
  # external-ip takes an address, not a name (DDNS): resolve it
  extip=$PUBLIC
  [[ $extip =~ ^[0-9.]+$ ]] || extip=$(getent ahostsv4 "$PUBLIC" 2>/dev/null | awk 'NR==1{print $1}')
  if [[ -n "$extip" ]]; then echo "external-ip=${extip}" >>/etc/turnserver.conf; fi
  chown root:turnserver /etc/turnserver.conf 2>/dev/null && chmod 640 /etc/turnserver.conf
  # A DDNS name: coturn's external-ip follows it (every 5 minutes), as the server's own voice address does
  if [[ ! $PUBLIC =~ ^[0-9.]+$ ]]; then
    cat >/usr/local/sbin/spectrevoice-turnip <<'TIP'
#!/usr/bin/env bash
# coturn's external-ip follows the DDNS name in SV_PUBLIC_IP (run by spectrevoice-turnip.timer)
set -euo pipefail
name=$(grep -E '^SV_PUBLIC_IP=' /etc/spectrevoice/server.env | cut -d= -f2- || true)
[[ -n $name && ! $name =~ ^[0-9.]+$ ]] || exit 0
ip=$(getent ahostsv4 "$name" | awk 'NR==1{print $1}' || true)
[[ -n $ip ]] || exit 0
grep -qx "external-ip=${ip}" /etc/turnserver.conf && exit 0
sed -i '/^external-ip=/d' /etc/turnserver.conf
echo "external-ip=${ip}" >>/etc/turnserver.conf
systemctl restart coturn
TIP
    chmod 755 /usr/local/sbin/spectrevoice-turnip
    printf '[Unit]\nDescription=coturn external-ip from the DDNS name (Spectre Voice)\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/spectrevoice-turnip\n' >/etc/systemd/system/spectrevoice-turnip.service
    printf '[Unit]\nDescription=coturn external-ip from the DDNS name, every 5 minutes\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=5min\n[Install]\nWantedBy=timers.target\n' >/etc/systemd/system/spectrevoice-turnip.timer
    systemctl daemon-reload
    systemctl enable --now spectrevoice-turnip.timer >/dev/null 2>&1 || true
  fi
  sed -i 's/^#\?TURNSERVER_ENABLED=.*/TURNSERVER_ENABLED=1/' /etc/default/coturn 2>/dev/null || true
  systemctl enable --now coturn >/dev/null 2>&1 || true
  systemctl restart coturn || true
  setenv SV_TURN "turn:${DOMAIN}:3478"
  setenv SV_TURN_SECRET "$tsecret"
fi

# ---- Firewall ----
if [[ $FIRST -eq 1 && $FIREWALL -eq 1 && -n ${FW:-} ]]; then
  for p in $OPEN_TCP; do
    if [[ $FW == ufw ]]; then ufw allow "${p}/tcp" >/dev/null; else firewall-cmd -q --permanent --add-port="${p}/tcp"; fi
  done
  for p in $OPEN_UDP; do
    if [[ $FW == ufw ]]; then ufw allow "${p}/udp" >/dev/null; else firewall-cmd -q --permanent --add-port="${p/:/-}/udp"; fi
  done
  [[ $FW == firewalld ]] && firewall-cmd -q --reload
  say "$(t fwdone)"
fi

# ---- Automatic updates: the newest release from GitHub, checked (SHA256SUMS), daily around 4 AM ----
# (on by default, also for servers installed before; SV_AUTO_UPDATE=false in server.env stops them)
if [[ $AUTOUPD -eq 1 ]]; then
  cat >/usr/local/sbin/spectrevoice-update <<'UPD'
#!/usr/bin/env bash
# Spectre Voice Server: installs the newest release when there is one (run daily by spectrevoice-update.timer).
set -euo pipefail
REPO="Stefan3Dx/SpectreVoice-Server"
BIN=/usr/local/bin/spectrevoice-server
grep -qs '^SV_AUTO_UPDATE=false' /etc/spectrevoice/server.env && exit 0
ARCH=$(dpkg --print-architecture)
have=$($BIN version 2>/dev/null || echo 0)
tag=$(curl -fsSI "https://github.com/${REPO}/releases/latest" | awk -F/ 'tolower($1) ~ /^location:/ {print $NF}' | tr -d '\r')
want=${tag#v}
[[ -n $want && $want != "$have" ]] || exit 0
newest=$(printf '%s\n%s\n' "$have" "$want" | sort -V | tail -1)
[[ $newest == "$want" ]] || exit 0
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
base="https://github.com/${REPO}/releases/download/${tag}"
curl -fsSL "${base}/spectrevoice-server-linux-${ARCH}.tar.gz" -o "$tmp/s.tgz"
curl -fsSL "${base}/SHA256SUMS" -o "$tmp/SHA256SUMS"
(cd "$tmp" && grep "linux-${ARCH}.tar.gz" SHA256SUMS | sed "s|spectrevoice-server-linux-${ARCH}.tar.gz|s.tgz|" | sha256sum -c --quiet)
tar -xzf "$tmp/s.tgz" -C "$tmp"
install -m 755 "$tmp/spectrevoice-server" "$BIN.new" && mv -f "$BIN.new" "$BIN" # a running file is replaced, not written over
curl -fsSL "${base}/install.sh" -o /usr/local/sbin/spectrevoice-install.sh 2>/dev/null || true
systemctl restart spectrevoice
echo "Spectre Voice Server: ${have} -> ${want}"
UPD
  chmod 755 /usr/local/sbin/spectrevoice-update
  printf '[Unit]\nDescription=Spectre Voice Server update\nAfter=network-online.target\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/spectrevoice-update\n' >/etc/systemd/system/spectrevoice-update.service
  printf '[Unit]\nDescription=Spectre Voice Server update, daily\n[Timer]\nOnCalendar=*-*-* 04:00:00\nRandomizedDelaySec=1h\nPersistent=true\n[Install]\nWantedBy=timers.target\n' >/etc/systemd/system/spectrevoice-update.timer
  systemctl daemon-reload
  systemctl enable --now spectrevoice-update.timer >/dev/null 2>&1 || true
  [[ $FIRST -eq 1 ]] && say "$(t autodone)"
else
  # Off (asked, --no-auto-update, or SV_AUTO_UPDATE=false): kept off
  [[ -f $ENVF ]] && setenv SV_AUTO_UPDATE false
  [[ -f /etc/systemd/system/spectrevoice-update.timer ]] && { systemctl disable --now spectrevoice-update.timer >/dev/null 2>&1 || true; }
fi

# The system journal (the server's log: connections, uploads, moderation) kept 30 days at most (privacy policy)
if [[ ! -f /etc/systemd/journald.conf.d/spectrevoice.conf ]]; then
  install -d /etc/systemd/journald.conf.d
  printf '[Journal]\nMaxRetentionSec=30day\n' >/etc/systemd/journald.conf.d/spectrevoice.conf
  systemctl restart systemd-journald 2>/dev/null || true
fi

say "$(t start)"
systemctl daemon-reload
systemctl enable spectrevoice >/dev/null
systemctl restart spectrevoice

# ---- Done: the address, the port and the admin key ----
for _ in $(seq 1 20); do
  systemctl is-active --quiet spectrevoice && [[ -f "$DATA/claim.code" || -f "$DATA/spectre.db" ]] && break
  sleep 0.5
done
if ! systemctl is-active --quiet spectrevoice; then
  journalctl -u spectrevoice -n 30 --no-pager
  echo "$(t fail)"; exit 1
fi
sleep 1
claim=$(cat "$DATA/claim.code" 2>/dev/null || true)
# What's in force (an update shows the kept settings)
listen=$(grep -E '^SV_LISTEN=' "$ENVF" | cut -d= -f2- || true)
domain=$(grep -E '^SV_INVITE_BASE=https://' "$ENVF" | sed -E 's|^SV_INVITE_BASE=https://([^/]+).*|\1|' || true)
pub=$(grep -E '^SV_PUBLIC_IP=' "$ENVF" | cut -d= -f2- || true)
lport=${listen##*:}; lport=${lport:-7880}
udpport=$(grep -E '^SV_UDP_PORT=' "$ENVF" | cut -d= -f2- || true)
if [[ -n $domain ]]; then ADDR="$domain"; SHOWPORT="443 (HTTPS)"
else ADDR="${pub:-$LANIP}:${lport}"; SHOWPORT="${lport}"; fi
[[ -n $udpport ]] && SHOWPORT="$SHOWPORT · UDP ${udpport}"
echo
printf '\033[1;32m%s\033[0m\n' "════════════════════════════════════════════════════════════════════"
printf '\033[1;32m  %s\033[0m\n' "$(printf "$(t ready)" "$($BIN version 2>/dev/null || echo)")"
printf '\033[1;32m%s\033[0m\n' "════════════════════════════════════════════════════════════════════"
row "$(t r_addr):" "$ADDR" '\033[1m'
row "$(t r_port):" "$SHOWPORT" '\033[1m'
if [[ $FIRST -eq 1 ]]; then
  if [[ -n $SVPASS ]]; then row "$(t r_pass):" "$SVPASS" '\033[1m'; else row "$(t r_pass):" "$(t r_open)"; fi
fi
if [[ -n "$claim" ]]; then
  row "$(t r_key):" "$claim" '\033[1;33m'
  echo "  $(t r_how)"
else
  row "$(t r_key):" "$(t r_owned)"
fi
echo
[[ -z $domain && $MODE == 3 ]] && warn "$(t r_plain)"
[[ -n $domain && $FIRST -eq 1 ]] && echo "  $(t r_dns)"
[[ -n ${PORTS_TXT:-} ]] && row "$(t r_ports):" "$PORTS_TXT"
row "$(t r_set):" "${ENVF}  (systemctl restart spectrevoice)"
row "$(t r_logs):" "journalctl -u spectrevoice -f"
row "$(t r_upd):" "sudo bash install.sh --update"
