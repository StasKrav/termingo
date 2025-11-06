#!/bin/bash

TERMINAL="/usr/local/bin/termingo"

# Запускаем лаунчер и автоматически закрываем терминал через 2 секунды после его завершения
kitty --disable-server -e "bash -c '$TERMINAL; sleep 0.3'" --title "Terminal" --geometry=120x30
