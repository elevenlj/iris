import { SnakeGame } from "/snake-engine.mjs";

const game = new SnakeGame({ size: 20 });
const elements = {
  canvas: document.getElementById("game-board"),
  score: document.getElementById("score"),
  best: document.getElementById("best-score"),
  level: document.getElementById("level"),
  badge: document.getElementById("status-badge"),
  overlay: document.getElementById("game-overlay"),
  overlayTitle: document.getElementById("overlay-title"),
  overlayCopy: document.getElementById("overlay-copy"),
  overlayAction: document.getElementById("overlay-action"),
  pause: document.getElementById("pause-button"),
  restart: document.getElementById("restart-button"),
  liveStatus: document.getElementById("live-status"),
  frame: document.querySelector(".board-frame"),
};

const context = elements.canvas.getContext("2d");
const keyDirections = new Map([
  ["ArrowUp", "up"],
  ["w", "up"],
  ["W", "up"],
  ["ArrowDown", "down"],
  ["s", "down"],
  ["S", "down"],
  ["ArrowLeft", "left"],
  ["a", "left"],
  ["A", "left"],
  ["ArrowRight", "right"],
  ["d", "right"],
  ["D", "right"],
]);

let timer = null;
let bestScore = loadBestScore();
let touchStart = null;

function loadBestScore() {
  try {
    return Number.parseInt(localStorage.getItem("iris-snake-best") || "0", 10) || 0;
  } catch {
    return 0;
  }
}

function saveBestScore() {
  if (game.score <= bestScore) return;
  bestScore = game.score;
  try {
    localStorage.setItem("iris-snake-best", String(bestScore));
  } catch {
    // The game remains playable when storage is unavailable.
  }
}

function formatScore(value) {
  return String(value).padStart(2, "0");
}

function colors() {
  const styles = getComputedStyle(document.documentElement);
  return {
    board: styles.getPropertyValue("--forest").trim(),
    grid: styles.getPropertyValue("--board-grid").trim(),
    snake: styles.getPropertyValue("--leaf").trim(),
    head: styles.getPropertyValue("--leaf-bright").trim(),
    fruit: styles.getPropertyValue("--fruit").trim(),
  };
}

function resizeCanvas() {
  const rect = elements.canvas.getBoundingClientRect();
  const ratio = Math.min(window.devicePixelRatio || 1, 2);
  const width = Math.max(1, Math.round(rect.width * ratio));
  const height = Math.max(1, Math.round(rect.height * ratio));
  if (elements.canvas.width !== width || elements.canvas.height !== height) {
    elements.canvas.width = width;
    elements.canvas.height = height;
  }
  draw();
}

function roundedCell(x, y, cellSize, inset, radius) {
  const left = x * cellSize + inset;
  const top = y * cellSize + inset;
  const size = cellSize - inset * 2;
  context.beginPath();
  context.roundRect(left, top, size, size, radius);
  context.fill();
}

function drawGrid(cellSize, width, height, palette) {
  context.strokeStyle = palette.grid;
  context.lineWidth = 1;
  context.beginPath();
  for (let i = 1; i < game.size; i += 1) {
    const point = Math.round(i * cellSize) + 0.5;
    context.moveTo(point, 0);
    context.lineTo(point, height);
    context.moveTo(0, point);
    context.lineTo(width, point);
  }
  context.stroke();
}

function drawFood(cellSize, palette) {
  if (!game.food) return;
  const centerX = (game.food.x + 0.5) * cellSize;
  const centerY = (game.food.y + 0.54) * cellSize;
  context.fillStyle = palette.fruit;
  context.beginPath();
  context.arc(centerX, centerY, cellSize * 0.28, 0, Math.PI * 2);
  context.fill();
  context.strokeStyle = palette.snake;
  context.lineWidth = Math.max(2, cellSize * 0.08);
  context.beginPath();
  context.moveTo(centerX, centerY - cellSize * 0.26);
  context.quadraticCurveTo(centerX + cellSize * 0.08, centerY - cellSize * 0.43, centerX + cellSize * 0.24, centerY - cellSize * 0.32);
  context.stroke();
}

function drawSnake(cellSize, palette) {
  game.snake.forEach((part, index) => {
    context.fillStyle = index === 0 ? palette.head : palette.snake;
    const inset = index === 0 ? cellSize * 0.09 : cellSize * 0.13;
    roundedCell(part.x, part.y, cellSize, inset, cellSize * 0.23);
  });

  const head = game.snake[0];
  const direction = game.direction;
  const vertical = direction === "up" || direction === "down";
  const forward = direction === "left" || direction === "up" ? -1 : 1;
  context.fillStyle = palette.board;
  for (const side of [-1, 1]) {
    const eyeX = (head.x + 0.5 + (vertical ? side * 0.16 : forward * 0.18)) * cellSize;
    const eyeY = (head.y + 0.5 + (vertical ? forward * 0.18 : side * 0.16)) * cellSize;
    context.beginPath();
    context.arc(eyeX, eyeY, Math.max(1.4, cellSize * 0.055), 0, Math.PI * 2);
    context.fill();
  }
}

function draw() {
  const width = elements.canvas.width;
  const height = elements.canvas.height;
  if (!width || !height) return;
  const palette = colors();
  const cellSize = Math.min(width, height) / game.size;
  context.clearRect(0, 0, width, height);
  context.fillStyle = palette.board;
  context.fillRect(0, 0, width, height);
  drawGrid(cellSize, width, height, palette);
  drawFood(cellSize, palette);
  drawSnake(cellSize, palette);
}

function announce(message) {
  elements.liveStatus.textContent = message;
}

function render() {
  elements.score.textContent = formatScore(game.score);
  elements.best.textContent = formatScore(bestScore);
  elements.level.textContent = formatScore(game.level);
  elements.pause.disabled = game.status === "idle" || game.status === "gameover" || game.status === "won";
  elements.pause.textContent = game.status === "paused" ? "继续" : "暂停";

  const stateCopy = {
    idle: ["等待开始", "准备好了吗？", "控制小蛇吃下果实，尽可能刷新纪录。", "开始游戏"],
    running: [`第 ${game.level} 级`, "", "", ""],
    paused: ["已暂停", "稍作休息", "准备好后继续穿过林间小径。", "继续游戏"],
    gameover: ["游戏结束", "撞到了！", `本局得到 ${game.score} 分，再试一次刷新纪录。`, "再来一局"],
    won: ["完成挑战", "整片森林都属于你", `满分通关，共获得 ${game.score} 分。`, "再来一局"],
  };
  const [badge, title, copy, action] = stateCopy[game.status];
  elements.badge.textContent = badge;
  elements.overlay.hidden = game.status === "running";
  if (game.status !== "running") {
    elements.overlayTitle.textContent = title;
    elements.overlayCopy.textContent = copy;
    elements.overlayAction.textContent = action;
  }
  elements.canvas.setAttribute("aria-label", `贪吃蛇棋盘，${badge}，得分 ${game.score}`);
  draw();
}

function clearTimer() {
  if (timer !== null) window.clearTimeout(timer);
  timer = null;
}

function scheduleTick() {
  clearTimer();
  if (game.status !== "running") return;
  timer = window.setTimeout(runTick, game.delay);
}

function runTick() {
  const result = game.tick();
  if (result.type === "eat") {
    saveBestScore();
    announce(`吃到果实，当前得分 ${game.score}，第 ${game.level} 级`);
  } else if (result.type === "gameover") {
    saveBestScore();
    announce(`游戏结束，本局得分 ${game.score}`);
  } else if (result.type === "won") {
    saveBestScore();
    announce(`恭喜完成挑战，总得分 ${game.score}`);
  }
  render();
  scheduleTick();
}

function startGame() {
  if (game.status === "gameover" || game.status === "won") game.reset();
  game.start();
  announce("游戏开始");
  render();
  scheduleTick();
}

function restartGame() {
  clearTimer();
  game.reset();
  game.start();
  announce("游戏已重新开始");
  render();
  scheduleTick();
}

function togglePause() {
  if (!game.togglePause()) return;
  announce(game.status === "paused" ? "游戏已暂停" : "游戏继续");
  render();
  scheduleTick();
}

function move(direction) {
  if (game.status === "idle") startGame();
  if (game.status !== "running") return;
  game.setDirection(direction);
}

document.addEventListener("keydown", (event) => {
  const direction = keyDirections.get(event.key);
  if (direction) {
    event.preventDefault();
    move(direction);
    return;
  }
  if (event.code === "Space") {
    event.preventDefault();
    if (game.status === "idle") startGame();
    else togglePause();
  } else if (event.key === "Enter" && (game.status === "gameover" || game.status === "won")) {
    event.preventDefault();
    restartGame();
  }
});

document.querySelectorAll("[data-direction]").forEach((button) => {
  button.addEventListener("click", () => move(button.dataset.direction));
});

elements.overlayAction.addEventListener("click", () => {
  if (game.status === "paused") togglePause();
  else startGame();
});
elements.pause.addEventListener("click", togglePause);
elements.restart.addEventListener("click", restartGame);

elements.frame.addEventListener("pointerdown", (event) => {
  touchStart = { x: event.clientX, y: event.clientY };
});

elements.frame.addEventListener("pointerup", (event) => {
  if (!touchStart) return;
  const deltaX = event.clientX - touchStart.x;
  const deltaY = event.clientY - touchStart.y;
  touchStart = null;
  if (Math.max(Math.abs(deltaX), Math.abs(deltaY)) < 24) return;
  if (Math.abs(deltaX) > Math.abs(deltaY)) move(deltaX > 0 ? "right" : "left");
  else move(deltaY > 0 ? "down" : "up");
});

elements.frame.addEventListener("pointercancel", () => {
  touchStart = null;
});

const resizeObserver = new ResizeObserver(resizeCanvas);
resizeObserver.observe(elements.frame);
window.addEventListener("resize", resizeCanvas);
render();
resizeCanvas();
