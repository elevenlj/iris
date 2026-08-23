export const DIRECTIONS = Object.freeze({
  up: { x: 0, y: -1 },
  down: { x: 0, y: 1 },
  left: { x: -1, y: 0 },
  right: { x: 1, y: 0 },
});

const sameCell = (a, b) => a.x === b.x && a.y === b.y;

export class SnakeGame {
  constructor({ size = 20, random = Math.random } = {}) {
    if (size < 8) throw new RangeError("棋盘尺寸不能小于 8");
    this.size = size;
    this.random = random;
    this.reset();
  }

  reset() {
    const middle = Math.floor(this.size / 2);
    this.snake = [
      { x: middle, y: middle },
      { x: middle - 1, y: middle },
      { x: middle - 2, y: middle },
    ];
    this.direction = "right";
    this.pendingDirection = "right";
    this.score = 0;
    this.level = 1;
    this.status = "idle";
    this.food = this.spawnFood();
  }

  get delay() {
    return Math.max(70, 165 - (this.level - 1) * 12);
  }

  start() {
    if (this.status === "idle" || this.status === "paused") {
      this.status = "running";
      return true;
    }
    return false;
  }

  togglePause() {
    if (this.status === "running") {
      this.status = "paused";
      return true;
    }
    if (this.status === "paused") {
      this.status = "running";
      return true;
    }
    return false;
  }

  setDirection(next) {
    if (!DIRECTIONS[next]) return false;
    const currentVector = DIRECTIONS[this.direction];
    const nextVector = DIRECTIONS[next];
    const reverses = currentVector.x + nextVector.x === 0 && currentVector.y + nextVector.y === 0;
    if (reverses) return false;
    this.pendingDirection = next;
    return true;
  }

  tick() {
    if (this.status !== "running") return { type: "idle" };

    this.direction = this.pendingDirection;
    const vector = DIRECTIONS[this.direction];
    const head = this.snake[0];
    const nextHead = { x: head.x + vector.x, y: head.y + vector.y };
    const ate = sameCell(nextHead, this.food);
    const body = ate ? this.snake : this.snake.slice(0, -1);
    const hitWall = nextHead.x < 0 || nextHead.y < 0 || nextHead.x >= this.size || nextHead.y >= this.size;
    const hitBody = body.some((part) => sameCell(part, nextHead));

    if (hitWall || hitBody) {
      this.status = "gameover";
      return { type: "gameover", reason: hitWall ? "wall" : "body" };
    }

    this.snake.unshift(nextHead);
    if (!ate) {
      this.snake.pop();
      return { type: "move" };
    }

    this.score += 1;
    this.level = 1 + Math.floor(this.score / 4);
    this.food = this.spawnFood();
    if (!this.food) {
      this.status = "won";
      return { type: "won" };
    }
    return { type: "eat" };
  }

  spawnFood() {
    const occupied = new Set(this.snake.map(({ x, y }) => `${x}:${y}`));
    const available = [];
    for (let y = 0; y < this.size; y += 1) {
      for (let x = 0; x < this.size; x += 1) {
        if (!occupied.has(`${x}:${y}`)) available.push({ x, y });
      }
    }
    if (available.length === 0) return null;
    const index = Math.min(available.length - 1, Math.floor(this.random() * available.length));
    return available[index];
  }
}
