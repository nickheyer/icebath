# ISSUES

1. Every window's legends/keybinds need to be inspected. The following example is one of 30+ issues ive observed in only minutes of use: Open a new icebath tui, alt + a for new window prompt, pick sessions window, and I see these in the legend...
    `j/k select`: what the fuck, its like you arent even trying. j/k MOVES UP AND DOWN IT DOES NOT SELECT. SELECT IS MAKING A SELECTION NOT NAVIGATING A TABLE. 
    `enter open`: open???? open what????? THIS LITERALLY JUST FUCKING FORKS IT FROM WHAT I CAN TELL. 
    `f fork`: same as enter afaik. 
    `d delete`: seems to work.
    `/ search`: seems to work, but annoying that I have to press `esc` to stop searching. Also theres no fucking search bar, its just placing arbitrary text somewhere above the table as you type. 
    `r reload`: what???? reload what????? 
    `alt+w close`: WHY NOT JUST q you fuck. WHy do I need alt????? 

    additionally, holding shift while clicking a second row or moving up/down should multiselect. fork multiple opens N sessions, delete multiple deletes them, etc. 

2. Even the main view of the app, the chat session, it's total shit. 
    `ask about icebath - cmd/, dev/, pkg/, README.md` ?????? WHAT THE FUCK IS THIS. I cant think of something, but i'd almost prefer nothing here. or maybe a randomly selected quote from cyberpunk, bladerunner, matrix, etc. (short ones and stylized).
    `enter send`: no shit sherlock. How about you just float a send button on the end of the input row, aligned neatly to top right corner. Maybe make it change a bit on session state.
    `shift+enter newline`: I FUCKING TOLD YOU `\` is newline. HOW IS IT POSSIBLE BOTH ARE MAKING NEWLINES IN DIFFERENT WAYS. 
    `/ commands`: Dont need this either. This can be in the empty input placeholder, placed before the quote flavor text. 
    `ctrl+f search`: DOESNT FUCKING WORK, IT PULLS UP VSCODES SEARCH. YOU ARE CONSTANTLY IN CONFLICT WITH OTHER KEYBINDS. 
    `pgup/pgdown scroll`: ??????? WHY. How about you put a scroll bar on the side, with a timeline that looks nice, and you can put your pgup key on the top and pgdown on the bottom.

3. Theme does nothing when selected in palette. WHY.

4. EVERY SINGLE OTHER WINDOW OR UI CONTROL NEEDS A REVIEW AND REFACTOR.

5. IMPORTANT PRIORITY, DO THIS FIRST: IN THE PALETTE, WINDOW MENU, / AUTOCOMPLETE MENU, and elsewhere: WHEREEVER THERE IS A KEYBIND, IT MUST BE DERIVED FROM A SINGLE KEYBINDS CONFIG FILE SAVED ON HOST. IT ALSO MUST BE EDITABLE IN THE UI WHICH AUTO UPDATES THAT BIND IN THE UI AND THE CONFIG FILE. YOU WILL SUPPORT SINGLE AND COMBINATION KEYS OF ANY TYPE AND AN EVENT LIKE SYSTEM, SIMILAR TO RIVER WM OR HYPRLAND. COMPLETELY SERIALIZABLE FROM HUMAN READABLE FILE.
