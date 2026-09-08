//! Recursive-descent parser producing the AST.
//!
//! Precedence (low → high): or, and, comparisons, +- , */%, unary, primary.

use crate::lexer::{lex, Token};
use platform_common::EngineError;

#[derive(Debug, Clone, PartialEq)]
pub enum Expr {
    Number(f64),
    Str(String),
    Bool(bool),
    Variable(String),
    Binary {
        op: BinOp,
        left: Box<Expr>,
        right: Box<Expr>,
    },
    Unary {
        op: UnOp,
        operand: Box<Expr>,
    },
    Call {
        func: String,
        args: Vec<Expr>,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BinOp {
    Add,
    Sub,
    Mul,
    Div,
    Mod,
    Eq,
    Neq,
    Lt,
    Lte,
    Gt,
    Gte,
    And,
    Or,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UnOp {
    Neg,
    Not,
}

const MAX_DEPTH: usize = 64;
const MAX_NODES: usize = 4_096;

pub fn parse(input: &str) -> Result<Expr, EngineError> {
    let tokens = lex(input, 2_048).map_err(|e| EngineError::formula(input, e.message))?;
    let mut p = Parser { tokens, pos: 0, source: input, nodes: 0, depth: 0 };
    let expr = p.parse_or()?;
    if p.peek() != &Token::Eof {
        return Err(EngineError::formula(input, "unexpected trailing tokens"));
    }
    Ok(expr)
}

struct Parser<'a> {
    tokens: Vec<Token>,
    pos: usize,
    source: &'a str,
    nodes: usize,
    depth: usize,
}

impl<'a> Parser<'a> {
    fn err(&self, msg: impl Into<String>) -> EngineError {
        EngineError::formula(self.source, msg)
    }

    fn peek(&self) -> &Token {
        self.tokens.get(self.pos).unwrap_or(&Token::Eof)
    }

    fn next(&mut self) -> Token {
        let t = self.tokens.get(self.pos).cloned().unwrap_or(Token::Eof);
        self.pos += 1;
        t
    }

    fn count_node(&mut self) -> Result<(), EngineError> {
        self.nodes += 1;
        if self.nodes > MAX_NODES {
            return Err(self.err("expression too complex (node budget exceeded)"));
        }
        Ok(())
    }

    fn depth_guard<F>(&mut self, f: F) -> Result<Expr, EngineError>
    where
        F: FnOnce(&mut Self) -> Result<Expr, EngineError>,
    {
        self.count_node()?;
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            self.depth -= 1;
            return Err(self.err("expression too deeply nested (depth budget exceeded)"));
        }
        let r = f(self);
        self.depth -= 1;
        r
    }

    fn parse_or(&mut self) -> Result<Expr, EngineError> {
        let mut left = self.parse_and()?;
        while matches!(self.peek(), Token::Or) {
            self.next();
            let right = self.parse_and()?;
            left = Expr::Binary { op: BinOp::Or, left: Box::new(left), right: Box::new(right) };
        }
        Ok(left)
    }

    fn parse_and(&mut self) -> Result<Expr, EngineError> {
        let mut left = self.parse_cmp()?;
        while matches!(self.peek(), Token::And) {
            self.next();
            let right = self.parse_cmp()?;
            left = Expr::Binary { op: BinOp::And, left: Box::new(left), right: Box::new(right) };
        }
        Ok(left)
    }

    fn parse_cmp(&mut self) -> Result<Expr, EngineError> {
        let left = self.parse_additive()?;
        let op = match self.peek() {
            Token::Eq => Some(BinOp::Eq),
            Token::Neq => Some(BinOp::Neq),
            Token::Lt => Some(BinOp::Lt),
            Token::Lte => Some(BinOp::Lte),
            Token::Gt => Some(BinOp::Gt),
            Token::Gte => Some(BinOp::Gte),
            _ => None,
        };
        if let Some(op) = op {
            self.next();
            let right = self.parse_additive()?;
            return Ok(Expr::Binary { op, left: Box::new(left), right: Box::new(right) });
        }
        Ok(left)
    }

    fn parse_additive(&mut self) -> Result<Expr, EngineError> {
        let mut left = self.parse_term()?;
        loop {
            let op = match self.peek() {
                Token::Plus => BinOp::Add,
                Token::Minus => BinOp::Sub,
                _ => break,
            };
            self.next();
            let right = self.parse_term()?;
            left = Expr::Binary { op, left: Box::new(left), right: Box::new(right) };
            self.count_node()?;
        }
        Ok(left)
    }

    fn parse_term(&mut self) -> Result<Expr, EngineError> {
        let mut left = self.parse_unary()?;
        loop {
            let op = match self.peek() {
                Token::Star => BinOp::Mul,
                Token::Slash => BinOp::Div,
                Token::Percent => BinOp::Mod,
                _ => break,
            };
            self.next();
            let right = self.parse_unary()?;
            left = Expr::Binary { op, left: Box::new(left), right: Box::new(right) };
            self.count_node()?;
        }
        Ok(left)
    }

    fn parse_unary(&mut self) -> Result<Expr, EngineError> {
        match self.peek() {
            Token::Minus => {
                self.next();
                let operand = self.depth_guard(|p| p.parse_unary())?;
                Ok(Expr::Unary { op: UnOp::Neg, operand: Box::new(operand) })
            }
            Token::Not => {
                self.next();
                let operand = self.depth_guard(|p| p.parse_unary())?;
                Ok(Expr::Unary { op: UnOp::Not, operand: Box::new(operand) })
            }
            _ => self.parse_primary(),
        }
    }

    fn parse_primary(&mut self) -> Result<Expr, EngineError> {
        self.count_node()?;
        match self.next() {
            Token::Number(v) => Ok(Expr::Number(v)),
            Token::Str(s) => Ok(Expr::Str(s)),
            Token::LParen => {
                let inner = self.depth_guard(|p| p.parse_or())?;
                if self.next() != Token::RParen {
                    return Err(self.err("expected `)`"));
                }
                Ok(inner)
            }
            Token::Ident(name) => {
                if matches!(self.peek(), Token::LParen) {
                    self.next(); // consume (
                    let mut args = Vec::new();
                    if !matches!(self.peek(), Token::RParen) {
                        loop {
                            args.push(self.depth_guard(|p| p.parse_or())?);
                            match self.next() {
                                Token::Comma => continue,
                                Token::RParen => break,
                                _ => return Err(self.err("expected `,` or `)` in argument list")),
                            }
                        }
                    } else {
                        self.next(); // consume )
                    }
                    if args.len() > 16 {
                        return Err(self.err("too many function arguments"));
                    }
                    return Ok(Expr::Call { func: name, args });
                }
                // true/false arrive from lexer as numbers; idents are variables.
                Ok(Expr::Variable(name))
            }
            t => Err(self.err(format!("unexpected token {:?}", t))),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_precedence() {
        let e = parse("2 + 3 * 4").unwrap();
        match e {
            Expr::Binary { op: BinOp::Add, left, right } => {
                assert!(matches!(*right, Expr::Binary { op: BinOp::Mul, .. }));
                assert!(matches!(*left, Expr::Number(2.0)));
            }
            _ => panic!("expected add"),
        }
    }

    #[test]
    fn parses_keywords_and_calls() {
        assert!(matches!(parse("a and b or not c").unwrap(), Expr::Binary { op: BinOp::Or, .. }));
        assert!(matches!(parse("min(1, 2)").unwrap(), Expr::Call { .. }));
        assert!(matches!(parse("event.payload.points").unwrap(), Expr::Variable(v) if v == "event.payload.points"));
    }

    #[test]
    fn rejects_garbage() {
        assert!(parse("1 +").is_err());
        assert!(parse("((1)").is_err());
        assert!(parse("1 2").is_err());
    }
}
