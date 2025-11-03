#!/bin/bash

TERMGO="/usr/local/bin/termgo"

# Простой расчет позиции (работает в большинстве случаев)
X_POS=450
Y_POS=200

xfce4-terminal --disable-server \
    -e "bash -c '$TERMGO; sleep 0.3'" \
    --title "Terminal" \
    --geometry=120x30+$X_POS+$Y_POS
